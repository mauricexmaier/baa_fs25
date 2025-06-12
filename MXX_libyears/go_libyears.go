// go_libyears_k8s.go
//
// Usage:
//   go run go_libyears_k8s.go /path/to/moduleRoot
//
// Beispiel:
//   go run go_libyears_k8s.go /tmp/libyears-123/kubernetes
//
// Der Befehl muss INSIDE eines Go-Moduls ausgeführt werden
// (go mod download sollte fehlerfrei sein).

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"
)

var semverTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

type Mod struct {
	Path     string
	Version  string
	Time     *time.Time
	Indirect bool
	Main     bool
	Update   *struct {
		Version string
		Time    *time.Time
	}
}

func main() {
	if len(os.Args) != 2 {
		fmt.Println("Usage: go run go_libyears_k8s.go /path/to/moduleRoot")
		os.Exit(1)
	}
	modDir := filepath.Clean(os.Args[1])

	// go list -m -u -json all  ==> Current + Latest Info
	cmd := exec.Command("go", "list", "-mod=mod", "-m", "-u", "-json", "all")

	cmd.Dir = modDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "go list failed: %v\n", err)
		os.Exit(1)
	}

	dec := json.NewDecoder(bytes.NewReader(out))

	var (
		totalDirect int
		usedCount   int
		totalLag    float64
	)

	fmt.Printf("%-28s %-12s %-12s %8s\n", "Package", "Current", "Latest", "Lag(yr)")
	for dec.More() {
		var m Mod
		if err := dec.Decode(&m); err != nil {
			fmt.Fprintf(os.Stderr, "decode error: %v\n", err)
			os.Exit(1)
		}

		if m.Main || m.Indirect {
			continue // nur direkte Fremd-Module
		}
		totalDirect++

		// Wir brauchen: echte Tags + Release-Zeiten
		if m.Update == nil || m.Time == nil || m.Update.Time == nil ||
			!semverTag.MatchString(m.Version) || !semverTag.MatchString(m.Update.Version) {
			fmt.Printf("[SKIP] %-22s  keine verwertbare Release-Info\n", m.Path)
			continue
		}

		lagY := m.Update.Time.Sub(*m.Time).Hours() / 24 / 365.0
		totalLag += lagY
		usedCount++

		fmt.Printf("%-28s %-12s %-12s %8.2f\n",
			m.Path, m.Version, m.Update.Version, lagY)
	}

	// Zusammenfassung
	if usedCount == 0 {
		fmt.Println("Keine auswertbaren Dependencies gefunden.")
		return
	}
	fmt.Println()
	fmt.Printf("TOTAL Lag: %.2f  |  Ø %.2f  |  %d/%d direkte Dependencies ausgewertet\n",
		totalLag, totalLag/float64(usedCount), usedCount, totalDirect)
}
