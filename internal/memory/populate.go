package memory

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
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
func Populate(store *Store, cfg PopulateConfig) error {
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = 10000
	}
	if cfg.MaxFuncs <= 0 {
		cfg.MaxFuncs = 50000
	}
	if cfg.MaxFlows <= 0 {
		cfg.MaxFlows = 500
	}

	client := &http.Client{Timeout: 30 * time.Second}
	var totalFiles, totalFuncs, totalFlows int

	// --- Files: get all file paths (skeleton only, 1 query) ---
	fileRows, err := cypherQuery(client, cfg.GnBaseURL, cfg.Repo, fmt.Sprintf(
		`MATCH (f:File) WHERE f.filePath IS NOT NULL
		 RETURN f.filePath AS path
		 LIMIT %d`, cfg.MaxFiles))
	if err != nil {
		log.Printf("[populate] files query error: %v", err)
	} else {
		for _, row := range fileRows {
			path, _ := row["path"].(string)
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

	// --- Functions: get all function/type names + file locations ---
	// Skeleton only — callers/callees are discovered by the agent during real work
	funcRows, err := cypherQuery(client, cfg.GnBaseURL, cfg.Repo, fmt.Sprintf(
		`MATCH (n) WHERE n.startLine IS NOT NULL AND n.name IS NOT NULL AND n.filePath IS NOT NULL
		 RETURN n.name AS name, n.filePath AS file
		 LIMIT %d`, cfg.MaxFuncs))
	if err != nil {
		log.Printf("[populate] funcs query error: %v", err)
	} else {
		for _, row := range funcRows {
			name, _ := row["name"].(string)
			file, _ := row["file"].(string)
			if name == "" {
				continue
			}

			if err := store.SaveFunc(FuncMemory{
				Repo:    cfg.Repo,
				Name:    name,
				File:    file,
				Purpose: "(not yet explored by agent)",
				Source:  "auto",
			}); err != nil {
				log.Printf("[populate] save func %s: %v", name, err)
				continue
			}
			totalFuncs++
			if totalFuncs%1000 == 0 {
				log.Printf("[populate] %s: %d functions indexed...", cfg.Repo, totalFuncs)
			}
		}
	}

	// --- Flows: get execution flows from /api/processes ---
	flows, err := fetchProcesses(client, cfg.GnBaseURL, cfg.Repo, cfg.MaxFlows)
	if err != nil {
		log.Printf("[populate] flows error: %v", err)
	} else {
		for _, flow := range flows {
			if err := store.SaveFlow(FlowMemory{
				Repo:        cfg.Repo,
				Name:        flow.Name,
				Purpose:     flow.Purpose,
				Files:       flow.Files,
				EntryPoints: flow.EntryPoints,
				Source:      "auto",
			}); err != nil {
				log.Printf("[populate] save flow %s: %v", flow.Name, err)
				continue
			}
			totalFlows++
		}
	}

	log.Printf("[populate] %s: %d files, %d functions, %d flows", cfg.Repo, totalFiles, totalFuncs, totalFlows)
	return nil
}

// ---------------------------------------------------------------------------
// codegraph API helpers
// ---------------------------------------------------------------------------

type cypherResponse struct {
	Result []map[string]interface{} `json:"result"`
	Error  string                   `json:"error,omitempty"`
}

func cypherQuery(client *http.Client, baseURL, repo, cypher string) ([]map[string]interface{}, error) {
	body := map[string]interface{}{"cypher": cypher}
	if repo != "" {
		body["repo"] = repo
	}
	payload, _ := json.Marshal(body)
	resp, err := client.Post(baseURL+"/api/query", "application/json", strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("cypher %d: %s", resp.StatusCode, raw)
	}
	var cr cypherResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, err
	}
	if cr.Error != "" {
		return nil, fmt.Errorf("cypher: %s", cr.Error)
	}
	return cr.Result, nil
}

type flowData struct {
	Name        string
	Purpose     string
	Files       string
	EntryPoints string
}

func fetchProcesses(client *http.Client, baseURL, repo string, maxFlows int) ([]flowData, error) {
	// Step 1: list all processes from /api/processes
	u := baseURL + "/api/processes"
	if repo != "" {
		u += "?repo=" + url.QueryEscape(repo)
	}
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("processes %d: %s", resp.StatusCode, raw)
	}

	var listResp struct {
		Processes []struct {
			Label     string `json:"label"`
			StepCount int    `json:"stepCount"`
		} `json:"processes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("decode processes list: %w", err)
	}

	// Step 2: fetch detail for each process to get steps with file paths
	var flows []flowData
	for i, p := range listResp.Processes {
		if i >= maxFlows {
			break
		}
		detailURL := baseURL + "/api/process?name=" + url.QueryEscape(p.Label)
		if repo != "" {
			detailURL += "&repo=" + url.QueryEscape(repo)
		}
		detailResp, err := client.Get(detailURL)
		if err != nil {
			log.Printf("[populate] process detail %s: %v", p.Label, err)
			continue
		}

		var detail struct {
			Steps []struct {
				Name     string `json:"name"`
				FilePath string `json:"filePath"`
			} `json:"steps"`
		}
		if err := json.NewDecoder(detailResp.Body).Decode(&detail); err != nil {
			detailResp.Body.Close()
			log.Printf("[populate] decode process %s: %v", p.Label, err)
			continue
		}
		detailResp.Body.Close()

		var files []string
		var entryPoints []string
		seen := make(map[string]bool)
		for j, step := range detail.Steps {
			if step.FilePath != "" && !seen[step.FilePath] {
				seen[step.FilePath] = true
				files = append(files, step.FilePath)
			}
			if j == 0 && step.Name != "" {
				entryPoints = append(entryPoints, step.Name)
			}
		}
		flows = append(flows, flowData{
			Name:        p.Label,
			Purpose:     "(not yet explored by agent)",
			Files:       strings.Join(files, ", "),
			EntryPoints: strings.Join(entryPoints, ", "),
		})
	}
	return flows, nil
}

func escapeCypherStr(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`)
}
