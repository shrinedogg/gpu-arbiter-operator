package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MetricsQuerier returns the DCGM free-VRAM gauge for a given PromQL query.
//
// ok is false when the metric is absent or unparseable (mirroring the bash
// vram_free() which echoes -1 in those cases). The caller treats an unknown
// value as -1 and falls back to the vLLM/timeout ungate signals.
type MetricsQuerier interface {
	FreeVRAM(ctx context.Context, baseURL, query string, timeout time.Duration) (value int64, ok bool, err error)
}

// VictoriaMetrics is a Prometheus-compatible instant-query client used to read
// the DCGM_FI_DEV_FB_FREE gauge from VictoriaMetrics.
type VictoriaMetrics struct {
	// Client is the HTTP client used for the query. If nil, http.DefaultClient
	// is used.
	Client *http.Client
}

// FreeVRAM performs an instant query against /api/v1/query.
func (v VictoriaMetrics) FreeVRAM(ctx context.Context, baseURL, query string, timeout time.Duration) (int64, bool, error) {
	cl := v.Client
	if cl == nil {
		cl = http.DefaultClient
	}
	if timeout > 0 {
		// Bound the whole request; the bash call used --max-time 4.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	endpoint := strings.TrimRight(baseURL, "/") + "/api/v1/query"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, false, err
	}
	q := req.URL.Query()
	q.Set("query", query)
	req.URL.RawQuery = q.Encode()

	resp, err := cl.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, false, fmt.Errorf("victoriametrics query %q: status %s: %s", query, resp.Status, strings.TrimSpace(string(body)))
	}

	var pr promResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return 0, false, fmt.Errorf("decode victoriametrics response: %w", err)
	}

	if pr.Status != "success" || len(pr.Data.Result) == 0 {
		return 0, false, nil
	}

	// result[0].value[1] is the scalar string, e.g. "32119". Mirror the bash
	// truncation to an integer.
	samp := pr.Data.Result[0].Value
	if len(samp) < 2 {
		return 0, false, nil
	}
	raw := strings.Trim(string(samp[1]), "\"")
	if raw == "" {
		return 0, false, nil
	}
	// PromQL may return a float like "32119" or "3.2119e4".
	s := raw
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	n, perr := strconv.ParseInt(s, 10, 64)
	if perr != nil {
		return 0, false, nil
	}
	return n, true, nil
}

// promResponse models the subset of the Prometheus instant-query JSON
// envelope we care about. We avoid depending on the prometheus client
// library to keep the operator's module graph small.
type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string       `json:"resultType"`
		Result     []promSample `json:"result"`
	} `json:"data"`
}

// promSample is one series of a vector result. Value is [ts, "scalar"].
type promSample struct {
	Value []json.RawMessage `json:"value"`
}
