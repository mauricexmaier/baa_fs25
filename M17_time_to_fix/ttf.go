package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

/* ---------- Flags ---------- */

var (
	jsonFile = flag.String("json", "", "OSV JSON file")
	repoSlug = flag.String("repo", "", "owner/repo on GitHub")
	plat     = flag.String("plat", "", "libraries.io platform (npm, pypi …)")
	pkg      = flag.String("pkg", "", "package name on that platform")
)

const dateFmt = "2006-01-02 15:04"

/* ---------- Types ---------- */

// type osvFile struct {
// 	Vulns []struct {
// 		ID       string `json:"id"`
// 		Affected []struct {
// 			Ranges []struct {
// 				Type   string `json:"type"`
// 				Events []struct {
// 					Introduced string `json:"introduced,omitempty"`
// 					Fixed      string `json:"fixed,omitempty"`
// 				} `json:"events"`
// 			} `json:"ranges"`
// 		} `json:"affected"`
// 	} `json:"vulns"`
// }

type osvFile struct {
	Vulns []struct {
		ID string `json:"id"`

		// ➊  NEU: Severity in die Struktur aufnehmen
		EcosystemSpecific struct {
			Severity string `json:"severity"`
		} `json:"ecosystem_specific"`

		DatabaseSpecific struct {
			Severity       string    `json:"severity"`
			NVDPublishedAt time.Time `json:"nvd_published_at"`
		} `json:"database_specific"`

		Published string `json:"published"`

		Affected []struct {
			Ranges []struct {
				Type   string `json:"type"`
				Events []struct {
					Introduced string `json:"introduced,omitempty"`
					Fixed      string `json:"fixed,omitempty"`
				} `json:"events"`
			} `json:"ranges"`
		} `json:"affected"`
	} `json:"vulns"`
}

type row struct {
	id, severity       string
	introTag, fixTag   string
	introDate, fixDate *time.Time
	publishedDate      *time.Time
}

/* ---------- GitHub helper ---------- */

func ghTagDate(slug, tag string) (*time.Time, error) {
	tok := os.Getenv("GH_PAT")
	if tok == "" {
		return nil, nil
	}
	try := []string{tag, "v" + tag}
	for _, t := range try {
		u := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", slug, t)
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == 200 {
			var v struct {
				PublishedAt time.Time `json:"published_at"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
				return nil, err
			}
			return &v.PublishedAt, nil
		}
	}
	return nil, nil
}

/* ---------- libraries.io helper ---------- */

func libioDate(platform, name, ver string) (*time.Time, error) {
	key := os.Getenv("LIBIO_KEY")
	if key == "" {
		return nil, nil
	}
	u := fmt.Sprintf("https://libraries.io/api/%s/%s?api_key=%s", platform, name, key)
	resp, err := http.Get(u)
	if err != nil || resp.StatusCode != 200 {
		return nil, nil
	}
	var r struct {
		Versions []struct {
			Number      string    `json:"number"`
			PublishedAt time.Time `json:"published_at"`
		} `json:"versions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, nil
	}
	for _, v := range r.Versions {
		if v.Number == ver {
			return &v.PublishedAt, nil
		}
	}
	return nil, nil
}

/* ---------- main ---------- */

func main() {
	var ignored int
	flag.Parse()
	if *jsonFile == "" || *repoSlug == "" {
		fmt.Println("usage: go run ttf_fix.go -json osv.json -repo owner/repo [-plat npm -pkg express]")
		return
	}
	if *plat != "" && *pkg == "" {
		parts := strings.Split(*repoSlug, "/")
		*pkg = parts[len(parts)-1]
	}

	// load OSV
	f, err := os.Open(*jsonFile)
	if err != nil {
		panic(err)
	}
	var osv osvFile
	if err := json.NewDecoder(f).Decode(&osv); err != nil {
		panic(err)
	}

	// build rows
	var rows []row
	for _, v := range osv.Vulns {
		var fixes []string
		introForFix := map[string]string{} // fixTag -> introTag

		for _, aff := range v.Affected {
			for _, rg := range aff.Ranges {
				if rg.Type != "SEMVER" && rg.Type != "ECOSYSTEM" && rg.Type != "GIT" {
					continue // anderes Schema (git), überspringen
				}
				var curIntro string
				for _, ev := range rg.Events {
					if ev.Introduced != "" {
						curIntro = ev.Introduced
					}
					if ev.Fixed != "" {
						fixes = append(fixes, ev.Fixed)
						introForFix[ev.Fixed] = curIntro
					}
				}
			}
		}
		if len(fixes) == 0 {
			continue
		}
		// pick earliest fixed (smallest semver)
		sort.Slice(fixes, func(i, j int) bool {
			return semver.Compare("v"+fixes[i], "v"+fixes[j]) < 0
		})
		fix := fixes[0]
		intro := introForFix[fix]
		if intro == "0" { // treat "0" as unspecified
			intro = ""
		}

		sev := strings.ToUpper(v.EcosystemSpecific.Severity)
		if sev == "" {
			sev = strings.ToUpper(v.DatabaseSpecific.Severity)
		}

		var published *time.Time

		var published1 *time.Time
		var published2 *time.Time

		if !v.DatabaseSpecific.NVDPublishedAt.IsZero() {
			published1 = &v.DatabaseSpecific.NVDPublishedAt
		}
		if v.Published != "" {
			if t, err := time.Parse(time.RFC3339, v.Published); err == nil {
				published2 = &t
			}
		}

		// Nimm das kleinere (frühere) Datum
		if published1 != nil && published2 != nil {
			if published1.Before(*published2) {
				published = published1
			} else {
				published = published2
			}
		} else if published1 != nil {
			published = published1
		} else if published2 != nil {
			published = published2
		}

		rows = append(rows, row{
			id: v.ID, severity: sev, introTag: intro, fixTag: fix,
			publishedDate: published,
		})
	}

	/* ---- fetch dates ---- */
	for i := range rows {
		if rows[i].introTag != "" {
			rows[i].introDate, _ = ghTagDate(*repoSlug, rows[i].introTag)
			if rows[i].introDate == nil && *plat != "" {
				rows[i].introDate, _ = libioDate(*plat, *pkg, rows[i].introTag)
			}
		}
		rows[i].fixDate, _ = ghTagDate(*repoSlug, rows[i].fixTag)
		if rows[i].fixDate == nil && *plat != "" {
			rows[i].fixDate, _ = libioDate(*plat, *pkg, rows[i].fixTag)
		}
	}

	/* ---- output ---- */
	fmt.Printf("\n=== %s ===\n", *repoSlug)
	fmt.Printf("%-20s | %-6s | %-12s | %-12s | %-16s | %-16s | %-16s | %-10s | %-10s\n",
		"CVE-ID", "Sev", "Intro-Tag", "Fix-Tag", "Published", "Intro-Date", "Fix-Date", "ΔFix", "ΔExposure")
	fmt.Println(strings.Repeat("-", 112))

	var sum float64
	var cnt int
	var sumExp float64
	var cntExp int
	var skippedExp int
	for _, r := range rows {
		iDate := "not found"
		fDate := "not found"
		diffFix := "   n/a"
		diffExp := "   n/a"
		pubDate := "not found"

		if r.introDate != nil {
			iDate = r.introDate.Format(dateFmt)
		}
		if r.fixDate != nil {
			fDate = r.fixDate.Format(dateFmt)
		}

		validSeverity := r.severity == "HIGH" || r.severity == "CRITICAL" || r.severity == "MODERATE"

		// ΔFix
		if validSeverity && r.introDate != nil && r.fixDate != nil {
			d := r.fixDate.Sub(*r.introDate).Hours() / 24
			diffFix = fmt.Sprintf("%6.1f", d)
			sum += d
			cnt++
		} else if !validSeverity {
			ignored++
		}

		// ΔExp
		if validSeverity && r.publishedDate != nil && r.fixDate != nil {
			d := r.fixDate.Sub(*r.publishedDate).Hours() / 24
			pubDate = r.publishedDate.Format(dateFmt)
			if d >= 0 {
				diffExp = fmt.Sprintf("%6.1f", d)
				sumExp += d
				cntExp++
			} else {
				diffExp = "  < 0"
				skippedExp++
			}
		}

		fmt.Printf("%-20s | %-6s | %-12s | %-12s | %-16s | %-16s | %-16s | %6s | %6s\n",
			r.id, r.severity, r.introTag, r.fixTag, pubDate, iDate, fDate, diffFix, diffExp)
	}
	fmt.Println(strings.Repeat("-", 112))
	if cnt == 0 {
		fmt.Printf("Ø Time-to-Fix (ΔFix): n/a (0 CVEs)\n")
	} else {
		fmt.Printf("Ø Time-to-Fix (ΔFix): %.1f Tage (%d CVEs)\n", sum/float64(cnt), cnt)
	}
	if cntExp == 0 {
		fmt.Printf("Ø Exposure Window (ΔExposure): n/a (0 CVEs)\n")
	} else {
		fmt.Printf("Ø Exposure Window (ΔExposure): %.1f Tage (%d CVEs)\n", sumExp/float64(cntExp), cntExp)
	}
	if skippedExp > 0 {
		fmt.Printf("%d CVEs mit negativem Exposure Window ignoriert\n", skippedExp)
	}
	if ignored > 0 {
		fmt.Printf("%d CVEs nicht berücksichtigt (LOW oder keine Severity)\n", ignored)
	}
}
