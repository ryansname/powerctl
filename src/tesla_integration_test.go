//go:build tesla_tariff_integration

// Tesla TIME_OF_USE_SETTINGS / SITE_TARIFF wire format notes
// =========================================================
//
// The read endpoint (SITE_TARIFF) and the write endpoint (TIME_OF_USE_SETTINGS)
// use DIFFERENT JSON shapes for the same conceptual fields. Reading and replaying
// a tariff verbatim does NOT work — you must transform read-shape → write-shape.
//
//   Field             Read (SITE_TARIFF)        Write (TIME_OF_USE_SETTINGS)
//   -----             ------------------        ----------------------------
//   tou_periods band  "ON_PEAK": [ {...} ]      "ON_PEAK": {"periods": [ {...} ]}
//   energy_charges    "Summer":   {"ON_PEAK":x} "Summer":   {"rates": {"ON_PEAK":x}}
//   demand_charges    "ALL":      {"ALL": 0}    "ALL":      {"rates": {"ALL": 0}}
//
// Tesla silently drops malformed writes (returns 200 / no error, but the tariff
// on the device is unchanged). When called via HA REST API some malformed writes
// surface as a 500 "Server got itself in trouble"; via MQTT/Node-RED the same
// payload is silently dropped. Always verify by re-fetching after a write.
//
// Other constraints discovered while iterating:
//   - The "code" field can be any string; "(edited)" is appended automatically
//     by the Tesla app and is not required on writes.
//   - Custom season names (e.g. "ShoulderMay") are accepted as long as the same
//     name keys exist in seasons / energy_charges / demand_charges.
//   - Wrapping seasons (fromMonth > toMonth) are accepted, e.g. Summer Oct–Apr.
//   - Setting "currency": "USD" is accepted; the API does not seem to enforce
//     it against the account locale.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/joho/godotenv"
)

func TestMain(m *testing.M) {
	// Load .env from the repo root (one level up from src/).
	// Errors are ignored so the test still runs if vars are set in the shell.
	_ = godotenv.Load("../.env")
	os.Exit(m.Run())
}

const integrationSiteID = "2233628"

// haServiceCall calls a Home Assistant service via the REST API.
// Set returnResponse=true to get the service_response back.
func haServiceCall(t *testing.T, domain, service string, data map[string]any, returnResponse bool) map[string]any {
	t.Helper()
	haURL := os.Getenv("HA_URL")
	haToken := os.Getenv("HA_TOKEN")
	if haURL == "" || haToken == "" {
		t.Fatal("HA_URL and HA_TOKEN env vars required (set in .env or shell)")
	}

	body, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	url := fmt.Sprintf("%s/api/services/%s/%s", haURL, domain, service)
	if returnResponse {
		url += "?return_response=true"
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+haToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http call: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	if resp.StatusCode >= 400 {
		t.Fatalf("HA service call failed (%d): %s", resp.StatusCode, respBody)
	}

	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		t.Fatalf("unmarshal response: %v\nraw: %s", err, respBody)
	}
	return result
}

// teslaTariffAPI calls tesla_custom.api with path_vars pre-filled for the site.
func teslaTariffAPI(t *testing.T, command string, extra map[string]any, returnResponse bool) map[string]any {
	t.Helper()
	params := map[string]any{
		"path_vars": map[string]any{"site_id": integrationSiteID},
	}
	for k, v := range extra {
		params[k] = v
	}
	return haServiceCall(t, "tesla_custom", "api", map[string]any{
		"command":    command,
		"parameters": params,
	}, returnResponse)
}

// extractTariff navigates the HA REST API response envelope to find the tariff object.
func extractTariff(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	// REST API with return_response wraps in {"service_response": {"response": {...}}}
	if sr, ok := result["service_response"].(map[string]any); ok {
		if r, ok := sr["response"].(map[string]any); ok {
			return r
		}
	}
	// Some HA versions return {"response": {...}} directly
	if r, ok := result["response"].(map[string]any); ok {
		return r
	}
	// Or the tariff might be the result itself
	if _, hasCode := result["code"]; hasCode {
		return result
	}
	raw, _ := json.MarshalIndent(result, "", "  ")
	t.Fatalf("could not find tariff in response:\n%s", raw)
	return nil
}

// TestTeslaFetchCurrentTariff fetches the live tariff from Tesla and saves it to testdata/.
func TestTeslaFetchCurrentTariff(t *testing.T) {
	result := teslaTariffAPI(t, "SITE_TARIFF", nil, true)
	tariff := extractTariff(t, result)

	if err := os.MkdirAll("testdata", 0755); err != nil {
		t.Fatalf("create testdata dir: %v", err)
	}
	b, err := json.MarshalIndent(tariff, "", "  ")
	if err != nil {
		t.Fatalf("marshal tariff: %v", err)
	}
	const outFile = "testdata/tesla_tariff_backup.json"
	if err := os.WriteFile(outFile, b, 0644); err != nil {
		t.Fatalf("write %s: %v", outFile, err)
	}
	t.Logf("saved current tariff to %s (code=%v name=%v)", outFile, tariff["code"], tariff["name"])
}

// TestTeslaSendTOUTariff sends the force-sellback TOU tariff (same path as startDischarge)
// to confirm the write endpoint still works with a known-good simple structure.
func TestTeslaSendTOUTariff(t *testing.T) {
	tariff := buildTOUTariff(time.Now())
	teslaTariffAPI(t, "TIME_OF_USE_SETTINGS", map[string]any{
		"tou_settings": map[string]any{"tariff_content_v2": tariff},
	}, true)

	result := teslaTariffAPI(t, "SITE_TARIFF", nil, true)
	got := extractTariff(t, result)
	t.Logf("tariff after TOU write: code=%v name=%v", got["code"], got["name"])
}

// TestTeslaSendOctopusTariff sends the full multi-season Octopus tariff and verifies
// it was accepted by reading it back. Note: the saved tesla_tariff_backup.json is
// in the read-shape and CANNOT be replayed verbatim — the build* functions emit the
// write-shape required by the API (see header comment).
func TestTeslaSendOctopusTariff(t *testing.T) {
	tariff := buildOctopusTariff()
	wantName, _ := tariff["name"].(string)

	teslaTariffAPI(t, "TIME_OF_USE_SETTINGS", map[string]any{
		"tou_settings": map[string]any{"tariff_content_v2": tariff},
	}, true)

	result := teslaTariffAPI(t, "SITE_TARIFF", nil, true)
	got := extractTariff(t, result)
	gotName, _ := got["name"].(string)
	t.Logf("name after Octopus write: %q (wanted %q)", gotName, wantName)
	if gotName != wantName {
		t.Errorf("Octopus write did not land: got name %q, want %q", gotName, wantName)
	}
}
