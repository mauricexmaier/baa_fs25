// multi_mttu.go
//
// Analyse-Tool für Mean-Time-To-Update (MTTU) direkt in Git-Repos.
// Unterstützt drei Stopp-Kriterien:
//   --commits N   → exakt N jüngste Commits begehen
//   --changes N   → bricht ab, sobald N Änderungen der
//                   Datei (go.mod | package.json | requirements.txt) gefunden wurden
//   --days N      → alle Commits der letzten N Tage
//
// Genau **eine** dieser Optionen muss gesetzt sein (>0).
//
// Ökosysteme: npm | go | py
//
// go run multi_mttu.go --eco go --commits 100 https://github.com/gorilla/mux.git

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"golang.org/x/mod/semver"
)

// -----------------------------------------------------------------------------
// Globale Flags
// -----------------------------------------------------------------------------
var (
	eco          string
	maxCommits   int // Stop-Kriterium 1
	maxChanges   int // Stop-Kriterium 2 (neu)
	lookBackDays int // Stop-Kriterium 3
	verbose      bool
)

func init() {
	flag.StringVar(&eco, "eco", "", "Ökosystem: npm | go | py")
	flag.IntVar(&maxCommits, "commits", -1, "Genau N jüngste Commits analysieren")
	flag.IntVar(&maxChanges, "changes", -1, "Stoppt nach N Datei-Änderungen")
	flag.IntVar(&lookBackDays, "days", -1, "Historie X Tage zurück")
	flag.BoolVar(&verbose, "v", true, "Verbose Log")
}

// commitsTouchingFiles ruft 'git log --pretty=%H -- <pfad>' auf
// und liefert die Hashes (jüngster Commit zuletzt).
func commitsTouchingFiles(repoDir string, paths []string, since, until *time.Time) ([]string, error) {
	args := []string{"log", "--first-parent", "--reverse", "--pretty=%H"}
	if since != nil {
		args = append(args, fmt.Sprintf("--since=%s", since.Format(time.RFC3339)))
	}
	if until != nil {
		args = append(args, fmt.Sprintf("--until=%s", until.Format(time.RFC3339)))
	}
	args = append(args, "--")
	args = append(args, paths...)

	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	hashes := strings.Fields(string(out))

	return hashes, nil
}

func logChange(c *object.Commit, dep, oldV, newV string) {
	if !verbose {
		return
	}
	fmt.Printf("%s  %s  %-38s  %s → %s\n",
		c.Author.When.Format("2006-01-02"),
		c.Hash.String()[:7],
		dep, oldV, newV)
}

// Prüft, dass **genau** ein Stopp-Flag >0 ist
func validateScopeFlags() {
	active := 0
	if maxCommits > 0 {
		active++
	}
	if maxChanges > 0 {
		active++
	}
	if lookBackDays > 0 {
		active++
	}
	if active != 1 {
		log.Fatal("genau EINE der Optionen --commits, --changes oder --days setzen (positiver Wert)")
	}
}

// -----------------------------------------------------------------------------
// Datenstrukturen
// -----------------------------------------------------------------------------
type delay struct {
	Dep        string
	OldVer     string
	NewVer     string
	Days       float64
	CommitHash string
	CommitDate time.Time
}

func canon(v string) string {
	// Leerstring, wenn nicht semver-konform
	vTemp := semver.Canonical(v)
	if vTemp == "" && !strings.HasPrefix(v, "v") {
		v = "v" + v // 1.2.3 → v1.2.3
	}
	return semver.Canonical(v) // nochmal prüfen
}

// -----------------------------------------------------------------------------
// ---------- NPM-Helfer --------------------------------------------------------
// -----------------------------------------------------------------------------
type npmMeta struct {
	Time map[string]string `json:"time"`
}

type timeCache struct {
	data map[string]map[string]time.Time
}

func (c *timeCache) get(pkg, ver string) (time.Time, error) {
	if c.data == nil {
		c.data = map[string]map[string]time.Time{}
	}
	if m, ok := c.data[pkg]; ok {
		if t, ok2 := m[ver]; ok2 {
			return t, nil
		}
	}
	url := fmt.Sprintf("https://registry.npmjs.org/%s", pkg)
	resp, err := http.Get(url)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return time.Time{}, fmt.Errorf("npm api status %s", resp.Status)
	}
	body, _ := io.ReadAll(resp.Body)
	var meta npmMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		return time.Time{}, err
	}
	m := map[string]time.Time{}
	for v, raw := range meta.Time {
		t, err := time.Parse(time.RFC3339, raw)
		if err == nil {
			m[v] = t
		}
	}
	c.data[pkg] = m
	if t, ok := m[ver]; ok {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("kein Datum für %s@%s", pkg, ver)
}

var npmTimes timeCache

func npmVersions(js string) map[string]string {
	var root map[string]interface{}
	_ = json.Unmarshal([]byte(js), &root)
	out := map[string]string{}
	if v, ok := root["dependencies"]; ok {
		if m, ok2 := v.(map[string]interface{}); ok2 {
			for dep, raw := range m {
				if s, ok3 := raw.(string); ok3 {
					out[dep] = strings.TrimLeft(s, "^~>=< ")
				}
			}
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// ---------- GO-Helfer ---------------------------------------------------------
// -----------------------------------------------------------------------------
type goInfo struct {
	Time time.Time `json:"Time"`
}

var goCache = map[string]map[string]time.Time{}

func goRelTime(module, ver string) (time.Time, error) {
	if m, ok := goCache[module]; ok {
		if t, ok2 := m[ver]; ok2 {
			return t, nil
		}
	}
	url := fmt.Sprintf("https://proxy.golang.org/%s/@v/%s.info", module, ver)
	resp, err := http.Get(url)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return time.Time{}, fmt.Errorf("proxy %s", resp.Status)
	}
	b, _ := io.ReadAll(resp.Body)
	var info goInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return time.Time{}, err
	}
	if _, ok := goCache[module]; !ok {
		goCache[module] = map[string]time.Time{}
	}
	goCache[module][ver] = info.Time
	return info.Time, nil
}

var reqLine = regexp.MustCompile(`^[\t ]*([\w./\-]+)[\t ]+v[^\s]+`)

func goVersions(txt string) map[string]string {
	m := map[string]string{}
	inBlock := false
	scan := bufio.NewScanner(strings.NewReader(txt))
	for scan.Scan() {
		l := strings.TrimSpace(scan.Text())
		switch {
		case strings.HasPrefix(l, "require ("):
			inBlock = true
			continue
		case inBlock && l == ")":
			inBlock = false
			continue
		}
		if !inBlock && !strings.HasPrefix(l, "require ") {
			continue
		}
		l = strings.TrimPrefix(l, "require")
		if m1 := reqLine.FindStringSubmatch(l); len(m1) > 0 {
			parts := strings.Fields(strings.TrimSpace(l))
			if len(parts) >= 2 {
				m[parts[0]] = parts[1]
			}
		}
	}
	return m
}

// -----------------------------------------------------------------------------
// ---------- PY-Helfer ---------------------------------------------------------
// -----------------------------------------------------------------------------
var reqRx = regexp.MustCompile(`^([A-Za-z0-9_.\-]+)==([0-9A-Za-z.+\-]+)$`)
var iniRx = regexp.MustCompile(`(?m)^\s*install_requires\s*=\s*$`)
var depLineRx = regexp.MustCompile(`^\s*([A-Za-z0-9_.\-]+)([=<>!~]*[0-9A-Za-z.+\-]*)`)

func readFileFromCommit(c *object.Commit, name string) (string, error) {
	f, err := c.File(name)
	if err != nil || f == nil { // Datei fehlt
		return "", err
	}
	return f.Contents()
}

func cfgVersions(txt string) map[string]string {
	deps := map[string]string{}

	// Regular expressions
	keyRx := regexp.MustCompile(`^install_requires\s*=\s*(.*)$`)
	depLineRx := regexp.MustCompile(`^([^#;\s][^><=!~\s]*)\s*([><=!~].+)?$`)

	inBlock := false
	lines := strings.Split(txt, "\n")

	for _, raw := range lines {
		l := strings.TrimRight(raw, "\r\t ")

		// Section switch ends the block
		if strings.HasPrefix(l, "[") {
			inBlock = false
		}

		// Detect the key line
		if m := keyRx.FindStringSubmatch(l); m != nil {
			inBlock = true
			// Inline form: install_requires = pkg>=1.2.3, otherpkg
			if tail := strings.TrimSpace(m[1]); tail != "" {
				for _, part := range strings.Split(tail, ",") {
					addDep(depLineRx, deps, part)
				}
			}
			continue
		}

		// Consume indented list items
		if inBlock {
			if strings.TrimSpace(l) == "" { // blank line ends the list
				inBlock = false
				continue
			}
			// Only consider indented lines (at least one leading space)
			if len(raw) > 0 && (raw[0] == ' ' || raw[0] == '\t') {
				addDep(depLineRx, deps, strings.TrimSpace(l))
			} else {
				// a non‑indented key terminates the list
				inBlock = false
			}
		}
	}
	return deps
}

// addDep parses a single requirement line and, if it matches, adds it to the map.
func addDep(rx *regexp.Regexp, dst map[string]string, line string) {
	if line == "" || strings.HasPrefix(line, "#") {
		return
	}
	if mm := rx.FindStringSubmatch(line); len(mm) >= 3 {
		name := strings.ToLower(mm[1])
		ver := strings.TrimLeft(mm[2], "=<>!~ ")
		dst[name] = ver
	}
}

func pyVersions(txt string) map[string]string {
	m := map[string]string{}
	scan := bufio.NewScanner(strings.NewReader(txt))
	for scan.Scan() {
		l := strings.TrimSpace(scan.Text())
		if strings.HasPrefix(l, "#") || l == "" {
			continue
		}
		if m1 := reqRx.FindStringSubmatch(l); len(m1) == 3 {
			m[strings.ToLower(m1[1])] = m1[2]
		}
	}
	return m
}

type pypiResp struct {
	Releases map[string][]struct {
		UploadTimeISO8601 string `json:"upload_time_iso_8601"`
	} `json:"releases"`
}

var pypiCache = map[string]map[string]time.Time{}

func pyRel(pkg, ver string) (time.Time, error) {
	pkg = strings.ToLower(pkg)
	if m, ok := pypiCache[pkg]; ok {
		if t, ok2 := m[ver]; ok2 {
			return t, nil
		}
	}
	url := fmt.Sprintf("https://pypi.org/pypi/%s/json", pkg)
	resp, err := http.Get(url)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return time.Time{}, fmt.Errorf("pypi %s", resp.Status)
	}
	body, _ := io.ReadAll(resp.Body)
	var pr pypiResp
	if err := json.Unmarshal(body, &pr); err != nil {
		return time.Time{}, err
	}
	uploads := pr.Releases[ver]
	if len(uploads) == 0 {
		return time.Time{}, errors.New("keine uploads")
	}
	t, err := time.Parse(time.RFC3339, uploads[0].UploadTimeISO8601)
	if err != nil {
		return time.Time{}, err
	}
	if _, ok := pypiCache[pkg]; !ok {
		pypiCache[pkg] = map[string]time.Time{}
	}
	pypiCache[pkg][ver] = t
	return t, nil
}

// -----------------------------------------------------------------------------
// ---------- ANALYSER ----------------------------------------------------------
// -----------------------------------------------------------------------------
func analyzeNPM(repo string) ([]delay, error) {
	var since *time.Time
	if lookBackDays > 0 {
		t := time.Now().AddDate(0, 0, -lookBackDays)
		since = &t
	}
	paths := []string{"package.json"}
	hashes, err := commitsTouchingFiles(repo, paths, since, nil)
	if err != nil {
		return nil, err
	}
	if maxCommits > 0 && len(hashes) > maxCommits {
		hashes = hashes[:maxCommits]
	}

	r, err := git.PlainOpen(repo)
	if err != nil {
		return nil, err
	}
	prev := map[string]string{}
	out := []delay{}

	// r, err := git.PlainOpen(repo)
	// if err != nil {
	// 	return nil, err
	// }
	// file := "package.json"
	// opts := git.LogOptions{
	// 	PathFilter: func(p string) bool { return p == "package.json" },
	// } // kein Path-Filter
	// if lookBackDays > 0 {
	// 	since := time.Now().AddDate(0, 0, -lookBackDays)
	// 	opts.Since = &since
	// }
	// iter, err := r.Log(&opts)
	// if err != nil {
	// 	return nil, err
	// }
	// var commits []*object.Commit
	// _ = iter.ForEach(func(c *object.Commit) error { commits = append(commits, c); return nil })

	// // Jüngste zuerst
	// for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
	// 	commits[i], commits[j] = commits[j], commits[i]
	// }
	// if maxCommits > 0 && len(commits) > maxCommits {
	// 	commits = commits[:maxCommits]
	// }

	// prev := map[string]string{}
	// out := []delay{}

CommitLoop:
	for idx, h := range hashes {
		c, err := r.CommitObject(plumbing.NewHash(h))
		if err != nil {
			continue
		}
		blob, err := c.File("package.json")
		if err != nil || blob == nil {
			continue
		}
		cont, _ := blob.Contents()
		curr := npmVersions(cont)
		if idx == 0 {
			prev = curr
			continue
		}
		for dep, newV := range curr {
			oldV, ok := prev[dep]
			if !ok || oldV == newV {
				continue
			}
			old := canon(oldV)
			new := canon(newV)

			if old == "" || new == "" { // unbekanntes Format → überspringen
				continue
			}
			if semver.Compare(old, new) >= 0 { // neue Version ist nicht größer
				continue // => Downgrade / equal  ⇒ ignorieren
			}
			rel, err := npmTimes.get(dep, newV)
			if err != nil {
				continue
			}
			diff := c.Author.When.Sub(rel).Hours() / 24
			if diff < 0 || diff > 365 {
				continue
			}
			logChange(c, dep, oldV, newV)
			out = append(out, delay{Dep: dep, OldVer: oldV, NewVer: newV, Days: diff,
				CommitHash: c.Hash.String()[:7], CommitDate: c.Author.When})

			if maxChanges > 0 && len(out) >= maxChanges {
				break CommitLoop
			}
			prev[dep] = newV
		}
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// ---------- analyzeGo ---------------------------------------------------------
// -----------------------------------------------------------------------------
func analyzeGo(repo string) ([]delay, error) {
	var since *time.Time
	if lookBackDays > 0 {
		t := time.Now().AddDate(0, 0, -lookBackDays)
		since = &t
	}
	paths := []string{"go.mod"}
	hashes, err := commitsTouchingFiles(repo, paths, since, nil)
	if err != nil {
		return nil, err
	}
	if maxCommits > 0 && len(hashes) > maxCommits {
		hashes = hashes[:maxCommits]
	}

	r, err := git.PlainOpen(repo)
	if err != nil {
		return nil, err
	}
	prev := map[string]string{}
	out := []delay{}

CommitLoop:
	for idx, h := range hashes {
		c, err := r.CommitObject(plumbing.NewHash(h))
		if err != nil {
			continue
		}
		blob, err := c.File("go.mod")
		if err != nil || blob == nil {
			continue
		}
		cont, _ := blob.Contents()
		curr := goVersions(cont)
		if idx == 0 {
			prev = curr
			continue
		}
		for mod, newV := range curr {
			oldV, ok := prev[mod]
			if !ok || oldV == newV {
				continue
			}
			old := canon(oldV)
			new := canon(newV)
			if old == "" || new == "" { // unbekanntes Format → überspringen
				continue
			}
			if semver.Compare(old, new) >= 0 { // neue Version ist nicht größer
				continue // => Downgrade / equal  ⇒ ignorieren
			}
			rel, err := goRelTime(mod, newV)
			if err != nil {
				continue
			}
			diff := c.Author.When.Sub(rel).Hours() / 24
			if diff < 0 || diff > 365 {
				continue
			}
			logChange(c, mod, oldV, newV)
			out = append(out, delay{Dep: mod, OldVer: oldV, NewVer: newV, Days: diff,
				CommitHash: c.Hash.String()[:7], CommitDate: c.Author.When})

			if maxChanges > 0 && len(out) >= maxChanges {
				break CommitLoop
			}
			prev[mod] = newV
		}
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// ---------- analyzePy ---------------------------------------------------------
// -----------------------------------------------------------------------------
func analyzePy(repo string) ([]delay, error) {
	// r, err := git.PlainOpen(repo)
	// if err != nil {
	// 	return nil, err
	// }
	// file := "requirements.txt"
	// if _, err := os.Stat(filepath.Join(repo, file)); err != nil {
	// 	return nil, fmt.Errorf("%s fehlt", file)
	// }
	// opts := git.LogOptions{}
	// if lookBackDays > 0 {
	// 	since := time.Now().AddDate(0, 0, -lookBackDays)
	// 	opts.Since = &since
	// }
	// iter, err := r.Log(&opts)
	// if err != nil {
	// 	return nil, err
	// }
	// var commits []*object.Commit
	// _ = iter.ForEach(func(c *object.Commit) error { commits = append(commits, c); return nil })
	// for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
	// 	commits[i], commits[j] = commits[j], commits[i]
	// }
	// if maxCommits > 0 && len(commits) > maxCommits {
	// 	commits = commits[:maxCommits]
	// }

	// prev := map[string]string{}
	// out := []delay{}
	var since *time.Time
	if lookBackDays > 0 {
		t := time.Now().AddDate(0, 0, -lookBackDays)
		since = &t
	}
	paths := []string{"requirements.txt", "setup.cfg"}
	hashes, err := commitsTouchingFiles(repo, paths, since, nil)
	if err != nil {
		return nil, err
	}
	if maxCommits > 0 && len(hashes) > maxCommits {
		hashes = hashes[:maxCommits]
	}

	r, err := git.PlainOpen(repo)
	if err != nil {
		return nil, err
	}
	prev := map[string]string{}
	out := []delay{}

CommitLoop:
	for idx, h := range hashes {
		c, err := r.CommitObject(plumbing.NewHash(h))
		if err != nil {
			continue
		}
		// blob, err := c.File("requirements.txt")
		// if err != nil || blob == nil {
		// 	continue
		// }
		// cont, _ := blob.Contents()
		// curr := pyVersions(cont)

		curr := map[string]string{}

		// 1) requirements.txt
		if txt, err := readFileFromCommit(c, "requirements.txt"); err == nil && txt != "" {
			for k, v := range pyVersions(txt) {
				curr[k] = v
			}
		}

		// 2) setup.cfg
		if txt, err := readFileFromCommit(c, "setup.cfg"); err == nil && txt != "" {
			for k, v := range cfgVersions(txt) {
				// Werte aus setup.cfg überschreiben evtl. requirements-Eintrag
				curr[k] = v
			}
		}

		// Kein Dependency-Change in diesem Commit → überspringen
		if len(curr) == 0 {
			continue
		}

		if idx == 0 {
			prev = curr
			continue
		}
		for dep, newV := range curr {
			oldV, ok := prev[dep]

			if !ok || oldV == newV {
				continue
			}
			old := canon(oldV)
			new := canon(newV)
			if old == "" || new == "" { // unbekanntes Format → überspringen
				continue
			}
			if semver.Compare(old, new) >= 0 { // neue Version ist nicht größer
				continue // => Downgrade / equal  ⇒ ignorieren
			}
			rel, err := pyRel(dep, newV)
			if err != nil {
				continue
			}
			diff := c.Author.When.Sub(rel).Hours() / 24
			if diff < 0 || diff > 365 {
				continue
			}
			logChange(c, dep, oldV, newV)
			out = append(out, delay{Dep: dep, OldVer: oldV, NewVer: newV, Days: diff,
				CommitHash: c.Hash.String()[:7], CommitDate: c.Author.When})

			if maxChanges > 0 && len(out) >= maxChanges {
				break CommitLoop
			}
			prev[dep] = newV
		}
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// ---------- Repo-Handling & Utils --------------------------------------------
// -----------------------------------------------------------------------------
func getAnalyzer() (func(string) ([]delay, error), error) {
	switch eco {
	case "npm":
		return analyzeNPM, nil
	case "go":
		return analyzeGo, nil
	case "py", "python":
		return analyzePy, nil
	default:
		return nil, fmt.Errorf("unbekanntes Ökosystem %q – erlaubt: npm | go | py", eco)
	}
}

func repoDir(url string) string {
	base := filepath.Base(strings.TrimSuffix(url, ".git"))
	return "./" + base
}

func ensureRepo(url string) (string, error) {
	dir := repoDir(url)
	token := os.Getenv("GH_TOKEN")
	var auth *githttp.BasicAuth
	if token != "" {
		auth = &githttp.BasicAuth{Username: "token", Password: token}
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if verbose {
			log.Printf("Klonen %s → %s", url, dir)
		}
		_, err = git.PlainClone(dir, false, &git.CloneOptions{
			URL:      url,
			Auth:     auth,
			Progress: os.Stderr,
		})
		return dir, err
	}
	if verbose {
		log.Printf("Verwende vorhandenes Repo %s", dir)
	}
	return dir, nil
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range xs {
		sum += v
	}
	return sum / float64(len(xs))
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	m := len(xs) / 2
	if len(xs)%2 == 0 {
		return (xs[m-1] + xs[m]) / 2
	}
	return xs[m]
}

// -----------------------------------------------------------------------------
// ---------- main --------------------------------------------------------------
// -----------------------------------------------------------------------------
func main() {
	flag.Parse()
	if flag.NArg() < 1 {
		log.Fatal("Usage: go run multi_mttu.go --eco <npm|go|py> (--commits N | --changes N | --days N) <git-url>")
	}
	validateScopeFlags()

	repoURL := flag.Arg(0)
	dir, err := ensureRepo(repoURL)
	if err != nil {
		log.Fatal(err)
	}
	analyzer, err := getAnalyzer()
	if err != nil {
		log.Fatal(err)
	}
	delays, err := analyzer(dir)
	if err != nil {
		log.Fatal(err)
	}
	if len(delays) == 0 {
		log.Println("Keine Updates erkannt – möglicherweise keine direkten Dependencies oder Filter zu eng")
		return
	}

	vals := make([]float64, len(delays))
	for i, d := range delays {
		vals[i] = d.Days
	}

	// -------------------- Summary --------------------------------------------
	fmt.Printf("\nSummary für %s (%s)\n", repoURL, eco)
	switch {
	case maxCommits > 0:
		fmt.Printf("Rückblick              : genau %d Commits\n", maxCommits)
	case lookBackDays > 0:
		fmt.Printf("Rückblick              : letzte %d Tage\n", lookBackDays)
	case maxChanges > 0:
		fmt.Printf("Stop nach              : %d Datei-Änderungen\n", maxChanges)
	}
	fmt.Printf("Analysierte Updates    : %d\n", len(delays))
	fmt.Printf("MTTU-Mean              : %.1f Tage\n", mean(vals))
	fmt.Printf("MTTU-Median            : %.1f Tage\n", median(vals))

	sort.Slice(delays, func(i, j int) bool { return delays[i].Days > delays[j].Days })
	top := 10
	if len(delays) < top {
		top = len(delays)
	}
	fmt.Println("\nLangsamste Updates:")
	for i := 0; i < top; i++ {
		d := delays[i]
		fmt.Printf("%-40s %7.0f d  (%s → %s) [%s %s]\n",
			d.Dep, d.Days, d.OldVer, d.NewVer,
			d.CommitDate.Format("06-01-02"), d.CommitHash)
	}
}
