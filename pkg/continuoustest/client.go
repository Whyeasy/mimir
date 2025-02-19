// SPDX-License-Identifier: AGPL-3.0-only

package continuoustest

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-kit/log"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/grafana/dskit/flagext"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"

	util_math "github.com/grafana/mimir/pkg/util/math"
)

const (
	maxErrMsgLen = 256
)

// MimirClient is the interface implemented by a client used to interact with Mimir.
type MimirClient interface {
	// WriteSeries writes input series to Mimir. Returns the response status code and optionally
	// an error. The error is always returned if request was not successful (eg. received a 4xx or 5xx error).
	WriteSeries(ctx context.Context, series []prompb.TimeSeries) (statusCode int, err error)

	// QueryRange performs a query for the given range.
	QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (model.Matrix, error)
}

type ClientConfig struct {
	TenantID string

	WriteBaseEndpoint flagext.URLValue
	WriteBatchSize    int
	WriteTimeout      time.Duration

	ReadBaseEndpoint flagext.URLValue
	ReadTimeout      time.Duration
}

func (cfg *ClientConfig) RegisterFlags(f *flag.FlagSet) {
	f.StringVar(&cfg.TenantID, "tests.tenant-id", "anonymous", "The tenant ID to use to write and read metrics in tests.")

	f.Var(&cfg.WriteBaseEndpoint, "tests.write-endpoint", "The base endpoint on the write path. The URL should have no trailing slash. The specific API path is appended by the tool to the URL, for example /api/v1/push for the remote write API endpoint, so the configured URL must not include it.")
	f.IntVar(&cfg.WriteBatchSize, "tests.write-batch-size", 1000, "The maximum number of series to write in a single request.")
	f.DurationVar(&cfg.WriteTimeout, "tests.write-timeout", 5*time.Second, "The timeout for a single write request.")

	f.Var(&cfg.ReadBaseEndpoint, "tests.read-endpoint", "The base endpoint on the read path. The URL should have no trailing slash. The specific API path is appended by the tool to the URL, for example /api/v1/query_range for range query API, so the configured URL must not include it.")
	f.DurationVar(&cfg.ReadTimeout, "tests.read-timeout", 30*time.Second, "The timeout for a single read request.")
}

type Client struct {
	writeClient *http.Client
	readClient  v1.API
	cfg         ClientConfig
	logger      log.Logger
}

func NewClient(cfg ClientConfig, logger log.Logger) (*Client, error) {
	rt := http.DefaultTransport
	rt = &clientRoundTripper{tenantID: cfg.TenantID, rt: rt}

	// Ensure the required config has been set.
	if cfg.WriteBaseEndpoint.URL == nil {
		return nil, errors.New("the write endpoint has not been set")
	}
	if cfg.ReadBaseEndpoint.URL == nil {
		return nil, errors.New("the read endpoint has not been set")
	}

	apiCfg := api.Config{
		Address:      cfg.ReadBaseEndpoint.String(),
		RoundTripper: rt,
	}

	readClient, err := api.NewClient(apiCfg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create read client")
	}

	return &Client{
		writeClient: &http.Client{Transport: rt},
		readClient:  v1.NewAPI(readClient),
		cfg:         cfg,
		logger:      logger,
	}, nil
}

// QueryRange implements MimirClient.
func (c *Client) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (model.Matrix, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.ReadTimeout)
	defer cancel()

	value, _, err := c.readClient.QueryRange(ctx, query, v1.Range{
		Start: start,
		End:   end,
		Step:  step,
	})
	if err != nil {
		return nil, err
	}

	if value.Type() != model.ValMatrix {
		return nil, errors.New("was expecting to get a Matrix")
	}

	matrix, ok := value.(model.Matrix)
	if !ok {
		return nil, errors.New("failed to cast type to Matrix")
	}

	return matrix, nil
}

// WriteSeries implements MimirClient.
func (c *Client) WriteSeries(ctx context.Context, series []prompb.TimeSeries) (int, error) {
	lastStatusCode := 0

	// Honor the batch size.
	for len(series) > 0 {
		end := util_math.Min(len(series), c.cfg.WriteBatchSize)
		batch := series[0:end]
		series = series[end:]

		var err error
		lastStatusCode, err = c.sendWriteRequest(ctx, &prompb.WriteRequest{Timeseries: batch})
		if err != nil {
			return lastStatusCode, err
		}
	}

	return lastStatusCode, nil
}

func (c *Client) sendWriteRequest(ctx context.Context, req *prompb.WriteRequest) (int, error) {
	data, err := proto.Marshal(req)
	if err != nil {
		return 0, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	defer cancel()

	compressed := snappy.Encode(nil, data)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.cfg.WriteBaseEndpoint.String()+"/api/v1/push", bytes.NewReader(compressed))
	if err != nil {
		// Errors from NewRequest are from unparseable URLs, so are not
		// recoverable.
		return 0, err
	}
	httpReq.Header.Add("Content-Encoding", "snappy")
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("User-Agent", "mimir-continuous-test")
	httpReq.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")

	httpResp, err := c.writeClient.Do(httpReq)
	if err != nil {
		return 0, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode/100 != 2 {
		truncatedBody, err := io.ReadAll(io.LimitReader(httpResp.Body, maxErrMsgLen))
		if err != nil {
			return httpResp.StatusCode, errors.Wrapf(err, "server returned HTTP status %s and client failed to read response body", httpResp.Status)
		}

		return httpResp.StatusCode, fmt.Errorf("server returned HTTP status %s and body %q (truncated to %d bytes)", httpResp.Status, string(truncatedBody), maxErrMsgLen)
	}

	return httpResp.StatusCode, nil
}

type clientRoundTripper struct {
	tenantID string
	rt       http.RoundTripper
}

// RoundTrip add the tenant ID header required by Mimir.
func (rt *clientRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("X-Scope-OrgID", rt.tenantID)
	return rt.rt.RoundTrip(req)
}
