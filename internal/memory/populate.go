package memory

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// PopulateConfig controls auto-population behavior.
type PopulateConfig struct {
	Repo      string
	GnBaseURL string
	MaxFiles  int
	MaxFuncs  int
	MaxFlows  int
}

// Populate auto-fills memory from codegraph for the given repo.
// Only creates entries with source=auto; never overwrites source=agent entries.
//
// Files and function skeletons come from the codegraph sidecar's /api/files
// and /api/symbols endpoints. Execution-flow auto-population is no longer
// available: the MIT codegraph engine has no process/flow extraction (the
// old GitNexus /api/processes endpoint is gone). Flows are still captured at
// runtime by the agent via SaveFlowMemory + Subgraph snapshots.
func Populate(store *Store, cfg PopulateConfig) error {
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = 10000
	}
	if cfg.MaxFuncs <= 0 {
		cfg.MaxFuncs = 50000
	}

	client := &http.Client{Timeout: 30 * time.Second}
	var totalFiles, totalFuncs int

	// --- Files: list all file paths ---
	paths, err := fetchFiles(client, cfg.GnBaseURL, cfg.Repo, cfg.MaxFiles)
	if err != nil {
		log.Printf("[populate] files query error: %v", err)
	} else {
		for _, path := range paths {
			if path == "" {
				continue
			}
			if err := store.SaveFile(FileMemory{
				Repo:    cfg.Repo,
				Path:    path,
				Purpose: "(not yet explored by agent)",
				Source:  "auto",
			}); err != nil {
				log.Printf("[populate] save file %s: %v", path, err)
				continue
			}
			totalFiles++
			if totalFiles%500 == 0 {
				log.Printf("[populate] %s: %d files indexed...", cfg.Repo, totalFiles)
			}
		}
	}

	// --- Functions: list all named symbols + file locations ---
	// Skeleton only — callers/callees are discovered by the agent during real work.
	syms, err := fetchSymbols(client, cfg.GnBaseURL, cfg.Repo, cfg.MaxFuncs)
	if err != nil {
		log.Printf("[populate] funcs query error: %v", err)
	} else {
		for _, s := range syms {
			if s.Name == "" {
				continue
			}
			if err := store.SaveFunc(FuncMemory{
				Repo:    cfg.Repo,
				Name:    s.Name,
				File:    s.File,
				Purpose: "(not yet explored by agent)",
				Source:  "auto",
			}); err != nil {
				log.Printf("[populate] save func %s: %v", s.Name, err)
				continue
			}
			totalFuncs++
			if totalFuncs%1000 == 0 {
				log.Printf("[populate] %s: %d functions indexed...", cfg.Repo, totalFuncs)
			}
		}
	}

	log.Printf("[populate] %s: %d files, %d functions (flow auto-population disabled with the MIT engine)", cfg.Repo, totalFiles, totalFuncs)
	return nil
}

// ---------------------------------------------------------------------------
// codegraph sidecar API helpers
// ---------------------------------------------------------------------------

func postJSON(client *http.Client, baseURL, path string, body map[string]any, out any) error {
	payload, _ := json.Marshal(body)
	resp, err := client.Post(baseURL+path, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %d: %s", path, resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func fetchFiles(client *http.Client, baseURL, repo string, limit int) ([]string, error) {
	var out struct {
		Paths []string `json:"paths"`
		Error string   `json:"error,omitempty"`
	}
	if err := postJSON(client, baseURL, "/api/files", map[string]any{"repo": repo, "limit": limit}, &out); err != nil {
		return nil, err
	}
	if out.Error != "" {
		return nil, fmt.Errorf("files: %s", out.Error)
	}
	return out.Paths, nil
}

type symbolRow struct {
	Name string `json:"name"`
	File string `json:"file"`
}

func fetchSymbols(client *http.Client, baseURL, repo string, limit int) ([]symbolRow, error) {
	var out struct {
		Symbols []symbolRow `json:"symbols"`
		Error   string      `json:"error,omitempty"`
	}
	if err := postJSON(client, baseURL, "/api/symbols", map[string]any{"repo": repo, "limit": limit}, &out); err != nil {
		return nil, err
	}
	if out.Error != "" {
		return nil, fmt.Errorf("symbols: %s", out.Error)
	}
	return out.Symbols, nil
}
