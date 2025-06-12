// libyears_npm_trim.go – npm-Libyears, Caret/Tilde werden entfernt
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

type npmResp struct {
	Time     map[string]string `json:"time"`
	DistTags map[string]string `json:"dist-tags"`
}

var (
	rxExact = regexp.MustCompile(`^\d+\.\d+\.\d+(-[\w\.]+)?$`)
	client  = &http.Client{Timeout: 15 * time.Second}
)

func main() {
	if len(os.Args) != 2 {
		log.Fatalf("Usage: %s path/to/package.json", os.Args[0])
	}
	pkgJSON := os.Args[1]

	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	j, _ := os.ReadFile(pkgJSON)
	if err := json.Unmarshal(j, &pkg); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("%-25s %-10s %-10s %8s\n", "Package", "Current", "Latest", "Lag(yr)")
	total, count := 0.0, 0

	for name, verRaw := range pkg.Dependencies {
		// 1. Caret (^) oder Tilde (~) einfach abschneiden
		ver := strings.TrimLeft(verRaw, "^~")

		// 2. nur exakte Major.Minor.Patch akzeptieren
		if !rxExact.MatchString(ver) {
			continue // überspringe Ranges wie ">=" usw.
		}

		latest, lag, err := libyear(name, ver)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[SKIP] %-20s %v\n", name, err)
			continue
		}
		fmt.Printf("%-25s %-10s %-10s %8.2f\n", name, ver, latest, lag)
		total += lag
		count++
	}

	if count > 0 {
		fmt.Printf("\nTOTAL Lag: %.2f  |  Ø %.2f\n", total, total/float64(count))
	} else {
		fmt.Println("No dependencies with exact or trimmed versions found.")
	}
}

func libyear(pkg, usedVer string) (latestVer string, lag float64, err error) {
	resp, err := client.Get("https://registry.npmjs.org/" + url.PathEscape(pkg))
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		err = fmt.Errorf("HTTP %d", resp.StatusCode)
		return
	}

	var js npmResp
	if err = json.NewDecoder(resp.Body).Decode(&js); err != nil {
		return
	}

	usedTimeStr, ok := js.Time[usedVer]
	if !ok {
		err = fmt.Errorf("timestamp for %s@%s not found", pkg, usedVer)
		return
	}

	var newest string
	var newestTime time.Time
	for ver, t := range js.Time {
		if ver == "created" || ver == "modified" {
			continue
		}
		tt, _ := time.Parse(time.RFC3339, t)
		if tt.After(newestTime) {
			newestTime, newest = tt, ver
		}
	}
	latestVer, latestTimeStr := newest, newestTime.Format(time.RFC3339)
	if lag < 0 {
		lag = 0
	}

	usedTime, _ := time.Parse(time.RFC3339, usedTimeStr)
	latestTime, _ := time.Parse(time.RFC3339, latestTimeStr)
	lag = latestTime.Sub(usedTime).Hours() / 24 / 365.25
	return
}
