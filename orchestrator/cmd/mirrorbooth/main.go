// Management API for the MirrorBooth retrieval pipeline.
// Shaders are build-time Flutter assets — this server triggers pipeline runs
// and tracks their status. Shader delivery to Flutter happens via the file system
// (integrator writes .frag + updates mirror_filter.dart), not over HTTP.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg := loadConfig()

	db, err := sql.Open("sqlite", cfg.DBURL)
	if err != nil {
		slog.Error("open db", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := initDB(db); err != nil {
		slog.Error("init db", "err", err)
		os.Exit(1)
	}

	h := &handler{db: db, cfg: cfg}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)

	r.Post("/api/v1/pipeline/run",  h.handleRun)
	r.Get("/api/v1/pipeline/jobs",  h.handleListJobs)
	r.Get("/api/v1/pipeline/jobs/{id}", h.handleGetJob)
	r.Get("/api/v1/healthz",        h.handleHealth)

	slog.Info("mirrorbooth management API starting", "addr", cfg.Addr)
	if err := http.ListenAndServe(cfg.Addr, r); err != nil {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}

// PipelineRunRequest triggers a pipeline run for a given prompt.
type PipelineRunRequest struct {
	Prompt          string `json:"prompt"`
	Name            string `json:"name"`            // output shader name, e.g. "neon_bokeh"
	Label           string `json:"label"`
	Icon            string `json:"icon"`
	RequireCamera   bool   `json:"require_camera"`
	RequireAnimated bool   `json:"require_animated"`
	MaxMobileCost   int    `json:"max_mobile_cost"`
	Seed            int64  `json:"seed"`
}

type Job struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"` // pending|running|done|failed
	Request   string    `json:"request"`
	StartedAt time.Time `json:"started_at"`
	DoneAt    *time.Time `json:"done_at,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type handler struct {
	db  *sql.DB
	cfg config
}

func (h *handler) handleRun(w http.ResponseWriter, r *http.Request) {
	var req PipelineRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Prompt == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "prompt and name are required")
		return
	}
	if req.Icon == "" {
		req.Icon = "🎨"
	}

	jobID := uuid.NewString()
	reqJSON, _ := json.Marshal(req)

	_, err := h.db.ExecContext(r.Context(),
		`INSERT INTO jobs (id, status, request, started_at) VALUES (?, 'pending', ?, ?)`,
		jobID, string(reqJSON), time.Now().Unix(),
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	go h.runPipeline(jobID, req)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"job_id":   jobID,
		"status":   "pending",
		"poll_url": fmt.Sprintf("/api/v1/pipeline/jobs/%s", jobID),
	})
}

func (h *handler) runPipeline(jobID string, req PipelineRunRequest) {
	ctx := context.Background()

	h.setJobStatus(ctx, jobID, "running", "")

	args := []string{
		"-m", "mirrorbooth_retrieval.run",
		"--prompt", req.Prompt,
		"--name", req.Name,
		"--mirrorbooth-root", h.cfg.MirrorBoothRoot,
		"--icon", req.Icon,
	}
	if req.Label != "" {
		args = append(args, "--label", req.Label)
	}
	if req.RequireCamera {
		args = append(args, "--require-camera")
	}
	if req.RequireAnimated {
		args = append(args, "--require-animated")
	}

	cmd := exec.CommandContext(ctx, "python3", args...)
	cmd.Env = append(os.Environ(),
		"LITELLM_URL="+h.cfg.LiteLLMURL,
		"EMBED_URL="+h.cfg.EmbedURL,
		"CHROMA_URL="+h.cfg.ChromaURL,
		"EVALUATOR_URL="+h.cfg.EvaluatorURL,
	)
	out, err := cmd.CombinedOutput()

	if err != nil {
		slog.Error("pipeline failed", "job_id", jobID, "err", err, "output", string(out))
		h.setJobStatus(ctx, jobID, "failed", fmt.Sprintf("%v: %s", err, out))
		return
	}

	slog.Info("pipeline done", "job_id", jobID)
	h.setJobStatus(ctx, jobID, "done", "")
}

func (h *handler) setJobStatus(ctx context.Context, jobID, status, errMsg string) {
	now := time.Now().Unix()
	_, _ = h.db.ExecContext(ctx,
		`UPDATE jobs SET status = ?, error = ?, done_at = ? WHERE id = ?`,
		status, errMsg, now, jobID,
	)
}

func (h *handler) handleListJobs(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(),
		`SELECT id, status, request, started_at, done_at, error FROM jobs ORDER BY started_at DESC LIMIT 50`,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	defer rows.Close()

	var jobs []map[string]any
	for rows.Next() {
		var id, status, request, errStr string
		var startedAt int64
		var doneAt sql.NullInt64
		_ = rows.Scan(&id, &status, &request, &startedAt, &doneAt, &errStr)
		j := map[string]any{
			"id":         id,
			"status":     status,
			"request":    request,
			"started_at": time.Unix(startedAt, 0),
		}
		if doneAt.Valid {
			t := time.Unix(doneAt.Int64, 0)
			j["done_at"] = t
		}
		if errStr != "" {
			j["error"] = errStr
		}
		jobs = append(jobs, j)
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (h *handler) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	row := h.db.QueryRowContext(r.Context(),
		`SELECT id, status, request, started_at, done_at, error FROM jobs WHERE id = ?`, id,
	)
	var jid, status, request, errStr string
	var startedAt int64
	var doneAt sql.NullInt64
	if err := row.Scan(&jid, &status, &request, &startedAt, &doneAt, &errStr); err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	j := map[string]any{
		"id":         jid,
		"status":     status,
		"request":    request,
		"started_at": time.Unix(startedAt, 0),
	}
	if doneAt.Valid {
		j["done_at"] = time.Unix(doneAt.Int64, 0)
	}
	if errStr != "" {
		j["error"] = errStr
	}
	writeJSON(w, http.StatusOK, j)
}

func (h *handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": time.Now().UTC()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

type config struct {
	Addr           string
	DBURL          string
	MirrorBoothRoot string
	LiteLLMURL     string
	EmbedURL       string
	ChromaURL      string
	EvaluatorURL   string
}

func loadConfig() config {
	return config{
		Addr:            envOrDefault("MIRRORBOOTH_ADDR",    ":8090"),
		DBURL:           envOrDefault("MIRRORBOOTH_DB",      "/data/orchestrator.db"),
		MirrorBoothRoot: envOrDefault("MIRRORBOOTH_ROOT",   "/data/mirrorbooth"),
		LiteLLMURL:      envOrDefault("LITELLM_URL",        "http://dgx-spark:4001"),
		EmbedURL:        envOrDefault("EMBED_URL",          "http://dgx-spark:8005"),
		ChromaURL:       envOrDefault("CHROMA_URL",         "http://dgx-spark:8006"),
		EvaluatorURL:    envOrDefault("EVALUATOR_URL",      "http://dgx-spark:8081"),
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func initDB(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS jobs (
			id          TEXT PRIMARY KEY,
			status      TEXT NOT NULL,
			request     TEXT NOT NULL,
			started_at  INTEGER NOT NULL,
			done_at     INTEGER,
			error       TEXT DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_jobs_started ON jobs(started_at);
	`)
	return err
}
