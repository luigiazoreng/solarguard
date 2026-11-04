// Package growatt implements the read-only connector for Growatt inverters and plants.
package growatt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/domain"
)

// Config configures a Growatt connector instance.
type Config struct {
	BaseURL         string
	Token           string
	Client          *http.Client
	DefaultTimezone string
}

// Connector is a read-only client for the Growatt OpenAPI.
type Connector struct {
	baseURL         string
	token           string
	client          *http.Client
	defaultTimezone string
}

// New creates a new Growatt connector with default configuration.
func New(baseURL, token string, client *http.Client) *Connector {
	return NewWithConfig(Config{
		BaseURL:         baseURL,
		Token:           token,
		Client:          client,
		DefaultTimezone: "America/Sao_Paulo",
	})
}

// NewWithConfig creates a new Growatt connector with explicit configuration.
func NewWithConfig(cfg Config) *Connector {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.DefaultTimezone == "" {
		cfg.DefaultTimezone = "America/Sao_Paulo"
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	return &Connector{
		baseURL:         base,
		token:           cfg.Token,
		client:          cfg.Client,
		defaultTimezone: cfg.DefaultTimezone,
	}
}

// Kind returns the provider name.
func (c *Connector) Kind() string {
	return "growatt"
}

// MakeLimitKey builds a deterministic, stable rate limit key: endpoint + "?" + params sorted by name.
func MakeLimitKey(endpoint string, params map[string]string) string {
	if len(params) == 0 {
		return endpoint + "?"
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString(endpoint)
	sb.WriteByte('?')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(params[k])
	}
	return sb.String()
}

// Discovery returns the initial task to list plants.
func (c *Connector) Discovery() []connector.Task {
	params := map[string]string{
		"page":    "1",
		"perpage": "100",
	}
	return []connector.Task{
		{
			Endpoint:       "plant/list",
			Params:         params,
			LimitKey:       MakeLimitKey("plant/list", params),
			MinInterval:    5 * time.Minute,
			DailyBudgetKey: "plant/list",
			DailyBudget:    10,
			Priority:       20,
			Refreshable:    true,
		},
	}
}

// DailyTasks produces daily collection tasks for a plant and date.
// Creates day_energy only for inverter, max, sph, spa, and min.
// If a plant has devices but none of those kinds, it falls back to plant/energy for a synthetic plant device.
func (c *Connector) DailyTasks(plant domain.Plant, devices []domain.Device, date string) []connector.Task {
	var eligibleDevices []domain.Device
	for _, d := range devices {
		switch d.Kind {
		case "inverter", "max", "sph", "spa", "min":
			eligibleDevices = append(eligibleDevices, d)
		}
	}

	tz := plant.Timezone
	if tz == "" {
		tz = c.defaultTimezone
	}

	if len(eligibleDevices) == 0 {
		params := map[string]string{
			"plant_id":   plant.ExternalID,
			"start_date": date,
			"end_date":   date,
			"time_unit":  "day",
			"timezone":   tz,
		}
		return []connector.Task{
			{
				Endpoint:       "plant/energy",
				Params:         params,
				LimitKey:       MakeLimitKey("plant/energy", params),
				MinInterval:    5 * time.Minute,
				DailyBudgetKey: "plant/energy",
				DailyBudget:    10,
				Priority:       10,
			},
		}
	}

	tasks := make([]connector.Task, 0, len(eligibleDevices))
	for _, dev := range eligibleDevices {
		if dev.Kind == "min" {
			tasks = append(tasks, minDayTask(dev.ExternalID, plant.ExternalID, tz, date))
			continue
		}
		params := map[string]string{
			"device_sn": dev.ExternalID,
			"date":      date,
			"plant_id":  plant.ExternalID,
			"timezone":  tz,
		}
		tasks = append(tasks, connector.Task{
			Endpoint:    "device/inverter/day_energy",
			Params:      params,
			LimitKey:    MakeLimitKey("device/inverter/day_energy", params),
			MinInterval: 5 * time.Minute,
			Priority:    10,
		})
	}
	return tasks
}

// minDayTask reads one day from a MIN inverter. In production day_energy returns
// a false 0 for MIN inverters, while tlx_data returns records newest first, so
// the first record's eacToday is the day's total and one record is enough.
func minDayTask(sn, plantExternalID, tz, date string) connector.Task {
	params := map[string]string{
		"tlx_sn":      sn,
		"start_date":  date,
		"end_date":    date,
		"timezone_id": tz,
		"page":        "1",
		"perpage":     "1",
		"plant_id":    plantExternalID,
		"timezone":    tz,
	}
	return connector.Task{
		Endpoint:    "device/tlx/tlx_data",
		Params:      params,
		LimitKey:    MakeLimitKey("device/tlx/tlx_data", params),
		MinInterval: 5 * time.Minute,
		Priority:    10,
	}
}

type apiEnvelope struct {
	ErrorCode int             `json:"error_code"`
	ErrorMsg  string          `json:"error_msg"`
	Data      json.RawMessage `json:"data"`
}

// Whitelist of valid query parameters per Growatt endpoint.
var endpointAllowedParams = map[string][]string{
	"plant/list":                 {"page", "perpage"},
	"plant/details":              {"plant_id"},
	"plant/data":                 {"plant_id"},
	"plant/energy":               {"plant_id", "start_date", "end_date", "time_unit"},
	"plant/power":                {"plant_id", "date"},
	"device/list":                {"plant_id", "page", "perpage"},
	"device/inverter/day_energy": {"device_sn", "date"},
	"device/tlx/tlx_data":        {"tlx_sn", "start_date", "end_date", "timezone_id", "page", "perpage"},
}

// readOnlyPostEndpoints lists the only calls sent with POST. The provider
// requires POST for them, but they only read data. Every other call is a GET.
var readOnlyPostEndpoints = map[string]bool{
	"device/tlx/tlx_data": true,
}

// Execute performs one read-only provider call.
func (c *Connector) Execute(ctx context.Context, task connector.Task) (connector.Result, error) {
	relPath := strings.TrimPrefix(task.Endpoint, "/")
	if !strings.HasPrefix(relPath, "v1/") {
		relPath = "v1/" + relPath
	}
	rawURL := c.baseURL + "/" + relPath

	// Build query string using allowed params for the endpoint
	q := url.Values{}
	allowed, isKnown := endpointAllowedParams[strings.TrimPrefix(task.Endpoint, "/")]
	if isKnown {
		for _, param := range allowed {
			if v, ok := task.Params[param]; ok {
				q.Set(param, v)
			}
		}
	} else {
		for k, v := range task.Params {
			q.Set(k, v)
		}
	}

	method, reqBody := http.MethodGet, io.Reader(nil)
	if readOnlyPostEndpoints[strings.TrimPrefix(task.Endpoint, "/")] {
		method, reqBody = http.MethodPost, strings.NewReader(q.Encode())
	} else if encoded := q.Encode(); encoded != "" {
		rawURL += "?" + encoded
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, reqBody)
	if err != nil {
		return connector.Result{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("token", c.token)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return connector.Result{}, c.sanitizeError(fmt.Errorf("execute get %s: %w", task.Endpoint, err))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return connector.Result{}, c.sanitizeError(fmt.Errorf("read response body: %w", err))
	}

	res := connector.Result{Raw: body}
	body = toUTF8(body)

	// Envelope parsing
	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return res, c.sanitizeError(fmt.Errorf("unmarshal envelope: %w", err))
	}

	if env.ErrorCode == 10012 {
		return res, &connector.RateLimitError{RetryAfter: 5 * time.Minute}
	}
	if env.ErrorCode != 0 {
		return res, c.sanitizeError(fmt.Errorf("growatt error %d: %s", env.ErrorCode, env.ErrorMsg))
	}
	if resp.StatusCode != http.StatusOK {
		return res, c.sanitizeError(fmt.Errorf("growatt http status %d", resp.StatusCode))
	}

	cleanEndpoint := strings.TrimPrefix(task.Endpoint, "/")
	cleanEndpoint = strings.TrimPrefix(cleanEndpoint, "v1/")

	switch cleanEndpoint {
	case "plant/list":
		err = c.parsePlantList(task, env.Data, &res)
	case "plant/details":
		err = c.parsePlantDetails(task, env.Data, &res)
	case "device/list":
		err = c.parseDeviceList(task, env.Data, &res)
	case "device/inverter/day_energy":
		err = c.parseDayEnergy(task, body, env.Data, &res)
	case "device/tlx/tlx_data":
		err = c.parseMinDay(task, env.Data, &res)
	case "plant/energy":
		err = c.parsePlantEnergy(task, env.Data, &res)
	}

	if err != nil {
		return res, c.sanitizeError(err)
	}

	return res, nil
}

func (c *Connector) sanitizeError(err error) error {
	if err == nil || c.token == "" {
		return err
	}
	clean := strings.ReplaceAll(err.Error(), c.token, "[REDACTED]")
	return errors.New(clean)
}

func (c *Connector) parsePlantList(task connector.Task, data json.RawMessage, res *connector.Result) error {
	var payload struct {
		Plants []struct {
			PlantID   any    `json:"plant_id"`
			PlantName string `json:"plant_name"`
			Name      string `json:"name"`
		} `json:"plants"`
		Count any `json:"count"`
	}

	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &payload); err != nil {
			return fmt.Errorf("unmarshal plant list data: %w", err)
		}
	}

	page, _ := strconv.Atoi(task.Params["page"])
	if page < 1 {
		page = 1
	}
	perpage, _ := strconv.Atoi(task.Params["perpage"])
	if perpage < 1 {
		perpage = 100
	}
	count := parseNumberInt(payload.Count)

	for _, p := range payload.Plants {
		extID := parseNumberString(p.PlantID)
		name := p.PlantName
		if name == "" {
			name = p.Name
		}
		if extID == "" {
			continue
		}
		res.Plants = append(res.Plants, connector.PlantInfo{
			ExternalID: extID,
			Name:       name,
		})

		// Followup: plant/details
		pDetailsParams := map[string]string{"plant_id": extID}
		res.Followups = append(res.Followups, connector.Task{
			Endpoint:    "plant/details",
			Params:      pDetailsParams,
			LimitKey:    MakeLimitKey("plant/details", pDetailsParams),
			MinInterval: 5 * time.Minute,
			Priority:    15,
			Refreshable: true,
		})

		// Followup: device/list page 1
		devListParams := map[string]string{
			"plant_id": extID,
			"page":     "1",
			"perpage":  "100",
		}
		res.Followups = append(res.Followups, connector.Task{
			Endpoint:    "device/list",
			Params:      devListParams,
			LimitKey:    MakeLimitKey("device/list", devListParams),
			MinInterval: 5 * time.Minute,
			Priority:    15,
			Refreshable: true,
		})
	}

	// Followup: next page if count requires one
	if page*perpage < count {
		nextParams := map[string]string{
			"page":    strconv.Itoa(page + 1),
			"perpage": strconv.Itoa(perpage),
		}
		res.Followups = append(res.Followups, connector.Task{
			Endpoint:       "plant/list",
			Params:         nextParams,
			LimitKey:       MakeLimitKey("plant/list", nextParams),
			MinInterval:    5 * time.Minute,
			DailyBudgetKey: "plant/list",
			DailyBudget:    10,
			Priority:       20,
			Refreshable:    true,
		})
	}

	return nil
}

func (c *Connector) parsePlantDetails(task connector.Task, data json.RawMessage, res *connector.Result) error {
	var payload struct {
		PlantID   any    `json:"plant_id"`
		PlantName string `json:"plant_name"`
		Name      string `json:"name"`
		Timezone  any    `json:"timezone"`
		Latitude  any    `json:"latitude"`
		Longitude any    `json:"longitude"`
		PeakPower any    `json:"peak_power"`
	}

	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &payload); err != nil {
			return fmt.Errorf("unmarshal plant details: %w", err)
		}
	}

	extID := parseNumberString(payload.PlantID)
	if extID == "" {
		extID = task.Params["plant_id"]
	}
	name := payload.PlantName
	if name == "" {
		name = payload.Name
	}

	rawTZ := parseNumberString(payload.Timezone)
	tz := ParseTimezone(rawTZ, c.defaultTimezone)

	lat := parseNumberFloat(payload.Latitude)
	lon := parseNumberFloat(payload.Longitude)
	peakPower := parseNumberFloat(payload.PeakPower)

	res.Plants = append(res.Plants, connector.PlantInfo{
		ExternalID:  extID,
		Name:        name,
		Timezone:    tz,
		Latitude:    lat,
		Longitude:   lon,
		PeakPowerKW: peakPower,
	})

	return nil
}

func mapDeviceType(typeVal int) string {
	switch typeVal {
	case 1:
		return "inverter"
	case 2:
		return "storage"
	case 3:
		return "other"
	case 4:
		return "max"
	case 5:
		return "sph"
	case 6:
		return "spa"
	case 7:
		return "min"
	case 8:
		return "pcs"
	case 9:
		return "hps"
	case 10:
		return "pbd"
	default:
		return "other"
	}
}

func (c *Connector) parseDeviceList(task connector.Task, data json.RawMessage, res *connector.Result) error {
	var payload struct {
		Devices []struct {
			DeviceSN       any `json:"device_sn"`
			SN             any `json:"sn"`
			Model          any `json:"model"`
			Type           any `json:"type"`
			Lost           any `json:"lost"`
			Status         any `json:"status"`
			LastUpdateTime any `json:"last_update_time"`
		} `json:"devices"`
		Count any `json:"count"`
	}

	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &payload); err != nil {
			return fmt.Errorf("unmarshal device list: %w", err)
		}
	}

	plantExtID := task.Params["plant_id"]
	page, _ := strconv.Atoi(task.Params["page"])
	if page < 1 {
		page = 1
	}
	perpage, _ := strconv.Atoi(task.Params["perpage"])
	if perpage < 1 {
		perpage = 100
	}
	count := parseNumberInt(payload.Count)

	for _, d := range payload.Devices {
		sn := parseNumberString(d.DeviceSN)
		if sn == "" {
			sn = parseNumberString(d.SN)
		}
		if sn == "" {
			continue
		}
		model := parseNumberString(d.Model)
		name := sn
		if model != "" {
			name = model + " " + sn
		}
		online := !parseLost(d.Lost)
		kind := mapDeviceType(parseNumberInt(d.Type))

		res.Devices = append(res.Devices, connector.DeviceInfo{
			PlantExternalID: plantExtID,
			ExternalID:      sn,
			Name:            name,
			Kind:            kind,
			Online:          &online,
			LastSeenLocal:   parseNumberString(d.LastUpdateTime),
		})
	}

	if page*perpage < count {
		nextParams := map[string]string{
			"plant_id": plantExtID,
			"page":     strconv.Itoa(page + 1),
			"perpage":  strconv.Itoa(perpage),
		}
		res.Followups = append(res.Followups, connector.Task{
			Endpoint:    "device/list",
			Params:      nextParams,
			LimitKey:    MakeLimitKey("device/list", nextParams),
			MinInterval: 5 * time.Minute,
			Priority:    15,
			Refreshable: true,
		})
	}

	return nil
}

func (c *Connector) parseDayEnergy(task connector.Task, rawBody, data json.RawMessage, res *connector.Result) error {
	// Shape 1: fields at root level (device_sn, dataloggerSn, data)
	// Shape 2: fields nested under data
	var root struct {
		DeviceSN string          `json:"device_sn"`
		Data     json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(rawBody, &root)

	var nested struct {
		DeviceSN string          `json:"device_sn"`
		Data     json.RawMessage `json:"data"`
	}
	if len(data) > 0 && string(data) != "null" {
		_ = json.Unmarshal(data, &nested)
	}

	deviceSN := root.DeviceSN
	if deviceSN == "" {
		deviceSN = nested.DeviceSN
	}
	if deviceSN == "" {
		deviceSN = task.Params["device_sn"]
	}

	// Determine data raw message
	var rawEnergyData json.RawMessage
	if len(root.Data) > 0 && !bytes.Equal(root.Data, []byte("null")) {
		// Could root.Data be an object with nested data?
		var inner struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(root.Data, &inner); err == nil && len(inner.Data) > 0 {
			rawEnergyData = inner.Data
		} else {
			rawEnergyData = root.Data
		}
	} else if len(nested.Data) > 0 && !bytes.Equal(nested.Data, []byte("null")) {
		rawEnergyData = nested.Data
	}

	if len(rawEnergyData) == 0 || bytes.Equal(rawEnergyData, []byte("null")) {
		return nil
	}
	rawVal := strings.Trim(string(rawEnergyData), "\"")
	if rawVal == "" || rawVal == "null" {
		return nil
	}
	valFloat, err := strconv.ParseFloat(rawVal, 64)
	if err != nil {
		return nil
	}
	valPtr := &valFloat

	date := task.Params["date"]
	tz := task.Params["timezone"]
	if tz == "" {
		tz = c.defaultTimezone
	}

	start, end, err := domain.LocalDay(date, tz)
	if err != nil {
		return fmt.Errorf("compute local day for %s (%s): %w", date, tz, err)
	}

	plantExtID := task.Params["plant_id"]

	res.Readings = append(res.Readings, connector.ReadingInfo{
		PlantExternalID:  plantExtID,
		DeviceExternalID: deviceSN,
		Metric:           domain.MetricEnergyKWh,
		PeriodStart:      start,
		PeriodEnd:        end,
		Value:            valPtr,
		RawValue:         rawVal,
	})

	return nil
}

// parseMinDay turns the newest record of a day into that day's reading. A day
// with no records produces no reading, so it surfaces as missing data.
func (c *Connector) parseMinDay(task connector.Task, data json.RawMessage, res *connector.Result) error {
	var payload struct {
		Datas []struct {
			Time     string          `json:"time"`
			EacToday json.RawMessage `json:"eacToday"`
		} `json:"datas"`
	}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &payload); err != nil {
			return fmt.Errorf("unmarshal min data: %w", err)
		}
	}
	date := task.Params["start_date"]
	if len(payload.Datas) == 0 || !strings.HasPrefix(payload.Datas[0].Time, date) {
		return nil
	}
	raw := strings.Trim(string(payload.Datas[0].EacToday), `"`)
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil
	}
	tz := task.Params["timezone"]
	if tz == "" {
		tz = c.defaultTimezone
	}
	start, end, err := domain.LocalDay(date, tz)
	if err != nil {
		return fmt.Errorf("compute local day for %s (%s): %w", date, tz, err)
	}
	res.Readings = append(res.Readings, connector.ReadingInfo{
		PlantExternalID:  task.Params["plant_id"],
		DeviceExternalID: task.Params["tlx_sn"],
		Metric:           domain.MetricEnergyKWh,
		PeriodStart:      start,
		PeriodEnd:        end,
		Value:            &value,
		RawValue:         raw,
	})
	return nil
}

func (c *Connector) parsePlantEnergy(task connector.Task, data json.RawMessage, res *connector.Result) error {
	var payload struct {
		Energys []struct {
			Date   string `json:"date"`
			Energy any    `json:"energy"`
		} `json:"energys"`
	}

	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &payload); err != nil {
			return fmt.Errorf("unmarshal plant energy: %w", err)
		}
	}

	plantExtID := task.Params["plant_id"]
	syntheticDeviceID := "plant:" + plantExtID

	// Synthetic plant device
	res.Devices = append(res.Devices, connector.DeviceInfo{
		PlantExternalID: plantExtID,
		ExternalID:      syntheticDeviceID,
		Name:            "Plant " + plantExtID,
		Kind:            "plant",
	})

	tz := task.Params["timezone"]
	if tz == "" {
		tz = c.defaultTimezone
	}

	for _, e := range payload.Energys {
		rawVal := parseNumberString(e.Energy)
		if rawVal == "" || rawVal == "null" {
			continue
		}
		valFloat, err := strconv.ParseFloat(rawVal, 64)
		if err != nil {
			continue
		}
		valPtr := &valFloat

		start, end, err := domain.LocalDay(e.Date, tz)
		if err != nil {
			return fmt.Errorf("compute local day for %s: %w", e.Date, err)
		}

		res.Readings = append(res.Readings, connector.ReadingInfo{
			PlantExternalID:  plantExtID,
			DeviceExternalID: syntheticDeviceID,
			Metric:           domain.MetricEnergyKWh,
			PeriodStart:      start,
			PeriodEnd:        end,
			Value:            valPtr,
			RawValue:         rawVal,
		})
	}

	return nil
}

// ParseTimezone normalizes Growatt time zone offsets and names into an IANA name loadable by time.LoadLocation.
// If the input cannot be loaded, defaultTZ is returned.
func ParseTimezone(raw, defaultTZ string) string {
	if defaultTZ == "" {
		defaultTZ = "America/Sao_Paulo"
	}
	tz := strings.Trim(strings.TrimSpace(raw), `"'`)
	if tz == "" {
		return defaultTZ
	}

	// If Go can directly load it, use it
	if _, err := time.LoadLocation(tz); err == nil {
		return tz
	}

	// Handle numeric offsets such as "-3", "+8", "-03:00", "+08", "-3.0", "3"
	tz = strings.TrimPrefix(tz, "GMT")
	tz = strings.TrimPrefix(tz, "UTC")
	offsetStr := strings.TrimSpace(tz)

	// Split by ":" if in HH:MM format
	colonIdx := strings.Index(offsetStr, ":")
	if colonIdx != -1 {
		offsetStr = offsetStr[:colonIdx]
	}

	hours, err := strconv.ParseFloat(offsetStr, 64)
	if err == nil {
		intHours := int(hours)
		if intHours == 0 {
			return "UTC"
		}
		// If offset is -3 and defaultTZ is America/Sao_Paulo (which is UTC-3), use America/Sao_Paulo
		if intHours == -3 && defaultTZ == "America/Sao_Paulo" {
			return "America/Sao_Paulo"
		}
		// In IANA tzdb:
		// Positive offset (ahead of UTC) is Etc/GMT-<N>
		// Negative offset (behind UTC) is Etc/GMT+<N>
		var gmtName string
		if intHours < 0 {
			gmtName = fmt.Sprintf("Etc/GMT+%d", -intHours)
		} else {
			gmtName = fmt.Sprintf("Etc/GMT-%d", intHours)
		}
		if _, err := time.LoadLocation(gmtName); err == nil {
			return gmtName
		}
	}

	return defaultTZ
}

// toUTF8 converts a response the provider sent in Latin-1. Without this, JSON
// decoding replaces accented letters in plant names. Result.Raw keeps the
// original bytes.
func toUTF8(body []byte) []byte {
	if utf8.Valid(body) {
		return body
	}
	converted, err := charmap.ISO8859_1.NewDecoder().Bytes(body)
	if err != nil {
		return body
	}
	return converted
}

// parseLost reads the device "lost" flag. The documentation describes a number
// (0 online, 1 disconnected), but production sends a boolean.
func parseLost(v any) bool {
	switch val := v.(type) {
	case bool:
		return val
	case string:
		s := strings.ToLower(strings.TrimSpace(val))
		return s == "1" || s == "true"
	default:
		return parseNumberInt(v) != 0
	}
}

func parseNumberString(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return strings.Trim(val, `"'`)
	case float64:
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(val, 10)
	case int:
		return strconv.Itoa(val)
	case json.Number:
		return val.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

func parseNumberInt(v any) int {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case int:
		return val
	case int64:
		return int(val)
	case float64:
		return int(val)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(val))
		return n
	case json.Number:
		n, _ := val.Int64()
		return int(n)
	default:
		return 0
	}
}

func parseNumberFloat(v any) *float64 {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case float64:
		return &val
	case int:
		f := float64(val)
		return &f
	case int64:
		f := float64(val)
		return &f
	case string:
		s := strings.TrimSpace(val)
		if s == "" {
			return nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err == nil {
			return &f
		}
		return nil
	case json.Number:
		f, err := val.Float64()
		if err == nil {
			return &f
		}
		return nil
	default:
		return nil
	}
}
