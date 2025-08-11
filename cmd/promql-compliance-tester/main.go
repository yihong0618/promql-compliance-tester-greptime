package main

import (
	"flag"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/cheggaaa/pb/v3"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/log"
	"github.com/promlabs/promql-compliance-tester/comparer"
	"github.com/promlabs/promql-compliance-tester/config"
	"github.com/promlabs/promql-compliance-tester/output"
	"github.com/promlabs/promql-compliance-tester/testcases"
)

func newPromAPI(targetConfig config.TargetConfig) (v1.API, error) {
	apiConfig := api.Config{Address: targetConfig.QueryURL}
	if len(targetConfig.Headers) > 0 || targetConfig.BasicAuthUser != "" {
		apiConfig.RoundTripper = roundTripperWithSettings{headers: targetConfig.Headers, basicAuthUser: targetConfig.BasicAuthUser, basicAuthPass: targetConfig.BasicAuthPass}
	}
	client, err := api.NewClient(apiConfig)
	if err != nil {
		return nil, errors.Wrapf(err, "creating Prometheus API client for %q: %v", targetConfig.QueryURL, err)
	}

	return v1.NewAPI(client), nil
}

type roundTripperWithSettings struct {
	headers       map[string]string
	basicAuthUser string
	basicAuthPass string
}

func (rt roundTripperWithSettings) RoundTrip(req *http.Request) (*http.Response, error) {
	// Per RoundTrip's documentation, RoundTrip should not modify the request,
	// except for consuming and closing the Request's Body.
	// TODO: Update the Go Prometheus client code to support adding headers to request.

	if rt.basicAuthUser != "" {
		req.SetBasicAuth(rt.basicAuthUser, rt.basicAuthPass)
	}

	for key, value := range rt.headers {
		req.Header.Add(key, value)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func main() {
	configFile := flag.String("config-file", "promql-compliance-tester.yml", "The path to the configuration file.")
	outputFormat := flag.String("output-format", "text", "The comparison output format. Valid values: [text, html, json]")
	outputHTMLTemplate := flag.String("output-html-template", "./output/example-output.html", "The HTML template to use when using HTML as the output format.")
	outputPassing := flag.Bool("output-passing", false, "Whether to also include passing test cases in the output.")
	flag.Parse()

	var outp output.Outputter
	switch *outputFormat {
	case "text":
		outp = output.Text
	case "html":
		var err error
		outp, err = output.HTML(*outputHTMLTemplate)
		if err != nil {
			log.Fatalf("Error reading output HTML template: %v", err)
		}
	case "json":
		outp = output.JSON
	case "tsv":
		outp = output.TSV
	default:
		log.Fatalf("Invalid output format %q", *outputFormat)
	}

	cfg, err := config.LoadFromFile(*configFile)
	if err != nil {
		log.Fatalf("Error loading configuration file: %v", err)
	}
	refAPI, err := newPromAPI(cfg.ReferenceTargetConfig)
	if err != nil {
		log.Fatalf("Error creating reference API: %v", err)
	}
	testAPI, err := newPromAPI(cfg.TestTargetConfig)
	if err != nil {
		log.Fatalf("Error creating test API: %v", err)
	}

	comp := comparer.New(refAPI, testAPI, cfg.QueryTweaks)

	end := getTime(cfg.QueryTimeParameters.EndTime, time.Now().UTC().Add(-2*time.Minute))
	start := end.Add(
		-getNonZeroDuration(cfg.QueryTimeParameters.RangeInSeconds, 10*time.Minute))
	resolution := getNonZeroDuration(
		cfg.QueryTimeParameters.ResolutionInSeconds, 10*time.Second)
	expandedTestCases := testcases.ExpandTestCases(cfg.TestCases, cfg.QueryTweaks, start, end, resolution)

	progressBar := pb.StartNew(len(expandedTestCases))
	results := make([]*comparer.Result, 0, len(cfg.TestCases))
	var errors []error
	var failedQueries []string
	for _, tc := range expandedTestCases {
		res, err := comp.Compare(tc)
		if err != nil {
			log.Errorf("Error running comparison: %v", err)
			errors = append(errors, err)
			failedQueries = append(failedQueries, tc.Query)
		} else {
			results = append(results, res)
		}
		progressBar.Increment()
	}
	progressBar.Finish()

	totalTests := len(expandedTestCases)
	successfulTests := len(results)
	errorCount := len(errors)
	successRate := float64(successfulTests) / float64(totalTests) * 100
	errorRate := float64(errorCount) / float64(totalTests) * 100

	log.Infof("Test execution summary:")
	log.Infof("  Total test cases: %d", totalTests)
	log.Infof("  Successful: %d (%.2f%%)", successfulTests, successRate)
	log.Infof("  Failed: %d (%.2f%%)", errorCount, errorRate)

	if len(errors) > 0 {
		log.Errorf("Found %d error(s) during test execution:", len(errors))
		for i, err := range errors {
			log.Errorf("  Error %d: %v", i+1, err)
		}

		log.Errorf("")
		log.Errorf("Failed queries summary:")
		log.Errorf("===================")
		for i, query := range failedQueries {
			log.Errorf("%d. %s", i+1, query)
		}
		log.Errorf("")
		log.Errorf("Complete PromQL queries that failed (copy-paste ready):")
		log.Errorf("=====================================================")
		for _, query := range failedQueries {
			log.Errorf("%s", query)
		}

		// 同时输出到标准输出，便于复制
		fmt.Printf("\n\n=== FAILED PROMQL QUERIES (Copy-paste ready) ===\n")
		for _, query := range failedQueries {
			fmt.Printf("%s\n", query)
		}
		fmt.Printf("=== END OF FAILED QUERIES ===\n\n")

		log.Fatalf("Test execution completed with %d error(s) - Error rate: %.2f%%", len(errors), errorRate)
	}

	outp(results, *outputPassing, cfg.QueryTweaks)
}

func getTime(timeStr string, defaultTime time.Time) time.Time {
	result, err := parseTime(timeStr)
	if err != nil {
		return defaultTime
	}
	return result
}

func getNonZeroDuration(
	seconds float64, defaultDuration time.Duration) time.Duration {
	if seconds == 0.0 {
		return defaultDuration
	}
	return time.Duration(seconds * float64(time.Second))
}

func parseTime(s string) (time.Time, error) {
	if t, err := strconv.ParseFloat(s, 64); err == nil {
		s, ns := math.Modf(t)
		return time.Unix(int64(s), int64(ns*float64(time.Second))).UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Time{}, errors.Errorf("cannot parse %q to a valid timestamp", s)
}
