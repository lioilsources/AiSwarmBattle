// Package pipelines provides an HTTP client for the evaluator service.
// The Python pipeline is invoked directly via exec.Command in cmd/mirrorbooth/main.go;
// this package is kept for future evaluator integration from Go.
package pipelines

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/lioilsources/shaderbattle/pkg/types"
)

// EvaluatorClient calls the evaluator HTTP service (EVALUATOR.md).
type EvaluatorClient struct {
	baseURL    string
	httpClient *http.Client
}

func NewEvaluatorClient(baseURL string) *EvaluatorClient {
	return &EvaluatorClient{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *EvaluatorClient) Evaluate(ctx context.Context, req *types.EvalRequest) (*types.EvalResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("eval: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/evaluate", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("eval: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("eval: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("eval: status %d", resp.StatusCode)
	}

	var result types.EvalResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("eval: decode: %w", err)
	}
	return &result, nil
}
