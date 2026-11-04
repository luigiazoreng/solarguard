package growatt

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/luigiazoreng/solarguard/internal/connector"
)

func TestLiveGrowatt(t *testing.T) {
	token := os.Getenv("GROWATT_TEST_TOKEN")
	if token == "" {
		t.Skip("GROWATT_TEST_TOKEN not set; skipping live Growatt integration test")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	c := New("https://test.growatt.com", token, client)
	ctx := context.Background()

	// 1. Read plant/list
	resList, err := c.Execute(ctx, connector.Task{
		Endpoint: "plant/list",
		Params:   map[string]string{"page": "1", "perpage": "10"},
	})
	if err != nil {
		t.Fatalf("live plant/list failed: %v", err)
	}

	if len(resList.Plants) == 0 {
		t.Log("no plants found on test server")
		return
	}

	// 2. Read plant/details for the first plant
	plantID := resList.Plants[0].ExternalID
	resDetails, err := c.Execute(ctx, connector.Task{
		Endpoint: "plant/details",
		Params:   map[string]string{"plant_id": plantID},
	})
	if err != nil {
		t.Fatalf("live plant/details failed: %v", err)
	}

	if len(resDetails.Plants) == 0 {
		t.Fatal("expected at least 1 plant in details response")
	}

	tz := resDetails.Plants[0].Timezone
	if tz == "" {
		t.Fatal("expected non-empty timezone in plant details")
	}
	if _, err := time.LoadLocation(tz); err != nil {
		t.Fatalf("timezone %q cannot be loaded: %v", tz, err)
	}
}
