// Package main implements the HTTP handler for the Global Payments Reporting API.
// It serves the same action based API as the Node.js server: /api/reports?action=...
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	serviceMu sync.Mutex
	service   *ReportingService
)

// reportingService returns the shared service, so the GP API access token is
// reused between requests. It is created on first use, which lets the docs and
// the config check answer before the credentials are set.
func reportingService() (*ReportingService, error) {
	serviceMu.Lock()
	defer serviceMu.Unlock()
	if service == nil {
		s, err := NewReportingService()
		if err != nil {
			return nil, err
		}
		service = s
	}
	return service, nil
}

func logError(context string, err error) {
	log.Printf("%s: %v", context, err)
}

func setJSONHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	setCORSHeaders(w)
}

func setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
}

func sendJSONResponse(w http.ResponseWriter, data interface{}, statusCode int) {
	setJSONHeaders(w)
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(data)
}

func handleError(w http.ResponseWriter, message string, statusCode int, errorCode string) {
	sendJSONResponse(w, map[string]interface{}{
		"success": false,
		"error": map[string]string{
			"code":      errorCode,
			"message":   message,
			"timestamp": now(),
		},
	}, statusCode)
}

// getRequestParams merges the query string with a JSON or form body
func getRequestParams(r *http.Request) map[string]string {
	params := map[string]string{}
	for key, values := range r.URL.Query() {
		if len(values) > 0 {
			params[key] = values[0]
		}
	}

	if r.Method != http.MethodPost {
		return params
	}

	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var body map[string]interface{}
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&body); err == nil {
			for key, value := range body {
				switch v := value.(type) {
				case string:
					params[key] = v
				case float64:
					params[key] = strconv.FormatFloat(v, 'f', -1, 64)
				case bool:
					params[key] = strconv.FormatBool(v)
				}
			}
		}
	} else if err := r.ParseForm(); err == nil {
		for key, values := range r.PostForm {
			if len(values) > 0 {
				params[key] = values[0]
			}
		}
	}
	return params
}

func validateRequiredParams(params map[string]string, required ...string) error {
	for _, name := range required {
		if params[name] == "" {
			return newValidationError("Missing required parameter: %s", name)
		}
	}
	return nil
}

func validateDates(params map[string]string) error {
	for _, name := range []string{"start_date", "end_date"} {
		if v := params[name]; v != "" {
			if _, err := time.Parse("2006-01-02", v); err != nil {
				return newValidationError("Invalid %s format. Use YYYY-MM-DD.", name)
			}
		}
	}
	return nil
}

// pick copies the named parameters that are set
func pick(params map[string]string, names ...string) map[string]string {
	out := map[string]string{}
	for _, name := range names {
		if v := params[name]; v != "" {
			out[name] = v
		}
	}
	return out
}

// limit reads a positive number, falling back to the default and capping it
func limit(params map[string]string, name string, def, max int) string {
	n, err := strconv.Atoi(params[name])
	if err != nil || n <= 0 {
		n = def
	}
	if n > max {
		n = max
	}
	return strconv.Itoa(n)
}

func page(params map[string]string) string {
	n, err := strconv.Atoi(params["page"])
	if err != nil || n <= 0 {
		n = 1
	}
	return strconv.Itoa(n)
}

var searchFilters = []string{"start_date", "end_date", "transaction_id", "payment_type", "status", "amount_min", "amount_max", "card_last_four"}

// handleReports serves /api/reports (and /reports) with an action parameter.
func handleReports(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		setJSONHeaders(w)
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		handleError(w, "Method not allowed", http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		return
	}

	params := getRequestParams(r)
	action := params["action"]
	if action == "" {
		// Path style: /reports/search, /reports/date-range, /reports/dispute/{id}, /reports/deposit/{id}
		path := strings.Trim(strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api"), "/reports"), "/")
		parts := strings.SplitN(path, "/", 2)
		action = strings.ReplaceAll(parts[0], "-", "_")
		if len(parts) == 2 && (action == "dispute" || action == "deposit") {
			params[action+"_id"] = parts[1]
			action += "_detail"
		}
	}

	result, err := runAction(r, action, params)
	if err != nil {
		var validation *validationError
		var apiErr *gpAPIError
		switch {
		case errors.As(err, &validation):
			handleError(w, err.Error(), http.StatusBadRequest, "VALIDATION_ERROR")
		case errors.As(err, &apiErr):
			handleError(w, err.Error(), http.StatusBadRequest, "API_ERROR")
		default:
			// Log unexpected errors on the server and return a generic message
			logError("Unexpected error in the reporting request", err)
			handleError(w, "An unexpected error occurred", http.StatusInternalServerError, "INTERNAL_ERROR")
		}
		return
	}

	if action == "export" {
		format := params["format"]
		if format == "csv" || format == "xml" {
			contentType := map[string]string{"csv": "text/csv", "xml": "application/xml"}[format]
			setCORSHeaders(w)
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", result["filename"]))
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, result["data"])
			return
		}
	}

	sendJSONResponse(w, result, http.StatusOK)
}

func runAction(r *http.Request, action string, params map[string]string) (map[string]interface{}, error) {
	ctx := r.Context()

	switch action {
	case "":
		return apiDocumentation(), nil
	case "config":
		return map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"sdk_status":             GetSDKConfigStatus(),
				"environment_validation": ValidateEnvironmentConfig(),
				"api_endpoints": map[string]string{
					"search":         "/api/reports?action=search",
					"detail":         "/api/reports?action=detail&transaction_id={id}",
					"settlement":     "/api/reports?action=settlement",
					"disputes":       "/api/reports?action=disputes",
					"dispute_detail": "/api/reports?action=dispute_detail&dispute_id={id}",
					"deposits":       "/api/reports?action=deposits",
					"deposit_detail": "/api/reports?action=deposit_detail&deposit_id={id}",
					"batches":        "/api/reports?action=batches",
					"declines":       "/api/reports?action=declines",
					"date_range":     "/api/reports?action=date_range",
					"export":         "/api/reports?action=export&format={json|csv|xml}",
					"summary":        "/api/reports?action=summary",
					"config":         "/api/reports?action=config",
				},
			},
			"timestamp": now(),
		}, nil
	}

	// Check the request before the credentials, as the other servers do
	var run func(*ReportingService) (map[string]interface{}, error)

	switch action {
	case "search":
		filters := pick(params, searchFilters...)
		filters["page"], filters["page_size"] = page(params), limit(params, "page_size", 10, 100)
		if err := validateDates(filters); err != nil {
			return nil, err
		}
		run = func(s *ReportingService) (map[string]interface{}, error) { return s.SearchTransactions(ctx, filters) }

	case "detail":
		if err := validateRequiredParams(params, "transaction_id"); err != nil {
			return nil, err
		}
		run = func(s *ReportingService) (map[string]interface{}, error) {
			return s.GetTransactionDetails(ctx, params["transaction_id"])
		}

	case "settlement":
		filters := pick(params, "start_date", "end_date")
		filters["page"], filters["page_size"] = page(params), limit(params, "page_size", 50, 100)
		if err := validateDates(filters); err != nil {
			return nil, err
		}
		run = func(s *ReportingService) (map[string]interface{}, error) { return s.GetSettlementReport(ctx, filters) }

	case "export":
		filters := pick(params, searchFilters...)
		format := params["format"]
		if format == "" {
			format = "json"
		}
		if format != "json" && format != "csv" && format != "xml" {
			return nil, newValidationError("Invalid format. Supported formats: json, csv, xml")
		}
		params["format"] = format
		if err := validateDates(filters); err != nil {
			return nil, err
		}
		run = func(s *ReportingService) (map[string]interface{}, error) {
			return s.ExportTransactions(ctx, filters, format)
		}

	case "summary":
		filters := pick(params, "start_date", "end_date")
		if err := validateDates(filters); err != nil {
			return nil, err
		}
		run = func(s *ReportingService) (map[string]interface{}, error) { return s.GetSummaryStats(ctx, filters) }

	case "disputes":
		filters := pick(params, "start_date", "end_date", "stage", "status")
		filters["page"], filters["page_size"] = page(params), limit(params, "page_size", 10, 100)
		if err := validateDates(filters); err != nil {
			return nil, err
		}
		run = func(s *ReportingService) (map[string]interface{}, error) { return s.GetDisputeReport(ctx, filters) }

	case "dispute_detail":
		if err := validateRequiredParams(params, "dispute_id"); err != nil {
			return nil, err
		}
		run = func(s *ReportingService) (map[string]interface{}, error) {
			return s.GetDisputeDetails(ctx, params["dispute_id"])
		}

	case "deposits":
		filters := pick(params, "start_date", "end_date", "deposit_id", "status")
		filters["page"], filters["page_size"] = page(params), limit(params, "page_size", 10, 100)
		if err := validateDates(filters); err != nil {
			return nil, err
		}
		run = func(s *ReportingService) (map[string]interface{}, error) { return s.GetDepositReport(ctx, filters) }

	case "deposit_detail":
		if err := validateRequiredParams(params, "deposit_id"); err != nil {
			return nil, err
		}
		run = func(s *ReportingService) (map[string]interface{}, error) {
			return s.GetDepositDetails(ctx, params["deposit_id"])
		}

	case "batches":
		filters := pick(params, "start_date", "end_date")
		if err := validateDates(filters); err != nil {
			return nil, err
		}
		run = func(s *ReportingService) (map[string]interface{}, error) { return s.GetBatchReport(ctx, filters) }

	case "declines":
		filters := pick(params, "start_date", "end_date", "payment_type", "amount_min", "amount_max", "card_last_four")
		filters["page"], filters["page_size"] = page(params), limit(params, "page_size", 10, 100)
		if err := validateDates(filters); err != nil {
			return nil, err
		}
		run = func(s *ReportingService) (map[string]interface{}, error) {
			return s.GetDeclinedTransactionsReport(ctx, filters)
		}

	case "date_range":
		filters := pick(params, "start_date", "end_date")
		filters["transaction_limit"] = limit(params, "transaction_limit", 100, 1000)
		filters["settlement_limit"] = limit(params, "settlement_limit", 50, 500)
		filters["dispute_limit"] = limit(params, "dispute_limit", 25, 100)
		filters["deposit_limit"] = limit(params, "deposit_limit", 25, 100)
		if err := validateDates(filters); err != nil {
			return nil, err
		}
		run = func(s *ReportingService) (map[string]interface{}, error) { return s.GetDateRangeReport(ctx, filters) }

	default:
		return nil, newValidationError("Invalid action: %s", action)
	}

	s, err := reportingService()
	if err != nil {
		return nil, err
	}
	return run(s)
}

// apiDocumentation is the answer to /api/reports without an action
func apiDocumentation() map[string]interface{} {
	dates := map[string]string{
		"start_date": "Start date (YYYY-MM-DD)",
		"end_date":   "End date (YYYY-MM-DD)",
	}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range dates {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	endpoint := func(url, method, description string, parameters map[string]string) map[string]interface{} {
		e := map[string]interface{}{"url": url, "method": method, "description": description}
		if parameters != nil {
			e["parameters"] = parameters
		}
		return e
	}

	return map[string]interface{}{
		"success": true,
		"data": map[string]interface{}{
			"name":        "Global Payments Reporting API",
			"version":     "1.0.0",
			"description": "RESTful API for Global Payments transaction reporting and analytics",
			"endpoints": map[string]interface{}{
				"search": endpoint("/api/reports?action=search", "GET/POST", "Search transactions with filters and pagination", with(map[string]string{
					"page":           "Page number (default: 1)",
					"page_size":      "Results per page (default: 10, max: 100)",
					"transaction_id": "Specific transaction ID",
					"payment_type":   "Payment type (sale, refund)",
					"status":         "Transaction status",
					"amount_min":     "Minimum amount",
					"amount_max":     "Maximum amount",
					"card_last_four": "Last 4 digits of card",
				})),
				"detail": endpoint("/api/reports?action=detail&transaction_id={id}", "GET", "Get detailed transaction information", map[string]string{
					"transaction_id": "Transaction ID (required)",
				}),
				"settlement": endpoint("/api/reports?action=settlement", "GET/POST", "Get settlement report", with(map[string]string{
					"page":      "Page number (default: 1)",
					"page_size": "Results per page (default: 50, max: 100)",
				})),
				"export": endpoint("/api/reports?action=export&format={json|csv|xml}", "GET/POST", "Export transaction data", map[string]string{
					"format":     "Export format (json, csv, or xml)",
					"...filters": "Same filters as search endpoint",
				}),
				"summary": endpoint("/api/reports?action=summary", "GET/POST", "Get summary statistics", with(nil)),
				"disputes": endpoint("/api/reports?action=disputes", "GET/POST", "Get dispute report", with(map[string]string{
					"page":      "Page number (default: 1)",
					"page_size": "Results per page (default: 10, max: 100)",
					"stage":     "Dispute stage",
					"status":    "Dispute status",
				})),
				"deposits": endpoint("/api/reports?action=deposits", "GET/POST", "Get deposit report", with(map[string]string{
					"page":       "Page number (default: 1)",
					"page_size":  "Results per page (default: 10, max: 100)",
					"deposit_id": "Specific deposit ID",
					"status":     "Deposit status",
				})),
				"batches": endpoint("/api/reports?action=batches", "GET/POST", "Get batch report", with(nil)),
				"declines": endpoint("/api/reports?action=declines", "GET/POST", "Get declined transactions report", with(map[string]string{
					"page":           "Page number (default: 1)",
					"page_size":      "Results per page (default: 10, max: 100)",
					"payment_type":   "Payment type",
					"amount_min":     "Minimum amount",
					"amount_max":     "Maximum amount",
					"card_last_four": "Last 4 digits of card",
				})),
				"date_range": endpoint("/api/reports?action=date_range", "GET/POST", "Get comprehensive date range report", with(map[string]string{
					"transaction_limit": "Max transactions (default: 100, max: 1000)",
					"settlement_limit":  "Max settlements (default: 50, max: 500)",
					"dispute_limit":     "Max disputes (default: 25, max: 100)",
					"deposit_limit":     "Max deposits (default: 25, max: 100)",
				})),
				"config": endpoint("/api/reports?action=config", "GET", "Get API configuration and status", nil),
			},
		},
		"timestamp": now(),
	}
}
