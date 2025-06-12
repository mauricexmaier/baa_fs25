// Berechnet Libyears für requirements.txt – zeigt Current, Latest, Lag
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"
)

type releaseInfo struct {
	Upload string `json:"upload_time_iso_8601"`
}
type pypiResponse struct {
	Info struct {
		Version string `json:"version"`
	} `json:"info"`
	Releases map[string][]releaseInfo `json:"releases"`
}

var (
	rx     = regexp.MustCompile(`^\s*([A-Za-z0-9._-]+)==([A-Za-z0-9._-]+)`)
	client = &http.Client{Timeout: 15 * time.Second}
)

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("Usage: %s requirements.txt [...] ", os.Args[0])
	}

	var total float64
	var count int

	fmt.Printf("%-25s %-10s %-10s %8s\n", "Package", "Current", "Latest", "Lag(yr)")

	for _, file := range os.Args[1:] {
		processFile(file, &total, &count)
	}

	if count > 0 {
		fmt.Printf("\nTOTAL Lag: %.2f  |  Ø %.2f\n", total, total/float64(count))
	} else {
		fmt.Println("No valid packages processed.")
	}
}

func processFile(path string, total *float64, count *int) {
	f, err := os.Open(path)
	if err != nil {
		log.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, cur, ok := parse(sc.Text())
		if !ok {
			continue
		}
		latest, lag, err := libyear(name, cur)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[SKIP] %-20s %v\n", name, err)
			continue
		}
		fmt.Printf("%-25s %-10s %-10s %8.2f\n", name, cur, latest, lag)
		*total += lag
		*count++
	}
}

func parse(line string) (name, ver string, ok bool) {
	m := rx.FindStringSubmatch(line)
	if len(m) == 3 {
		return m[1], m[2], true
	}
	return
}

func libyear(pkg, usedVer string) (latestVer string, lag float64, err error) {
	resp, err := client.Get("https://pypi.org/pypi/" + url.PathEscape(pkg) + "/json")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		err = fmt.Errorf("HTTP %d", resp.StatusCode)
		return
	}

	var js pypiResponse
	if err = json.NewDecoder(resp.Body).Decode(&js); err != nil {
		return
	}

	usedList, ok := js.Releases[usedVer]
	if !ok || len(usedList) == 0 {
		err = fmt.Errorf("no release info for %s %s", pkg, usedVer)
		return
	}
	latestVer = js.Info.Version
	latestList := js.Releases[latestVer]
	if len(latestList) == 0 {
		err = fmt.Errorf("no release info for latest %s", latestVer)
		return
	}

	usedTime, _ := time.Parse(time.RFC3339, usedList[0].Upload)
	latestTime, _ := time.Parse(time.RFC3339, latestList[0].Upload)
	lag = latestTime.Sub(usedTime).Hours() / 24 / 365.25
	return
}
