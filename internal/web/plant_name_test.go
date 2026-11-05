package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestManagePlantNameCorrection(t *testing.T) {
	st := newWebTestStore(t)
	p, _, _ := seedPlantWithDevices(t, st, "NAME", "Usina Exempl�")
	seedPlantWithDevices(t, st, "CLEAN", "Usina Exemplo")
	ts := startServer(t, st, time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC))
	client, token := sessionClient(t, ts.URL)
	const note = "Nome corrompido pela origem. Corrija aqui."
	body := getBody(t, ts.URL+"/manage")
	if strings.Count(body, note) != 1 {
		t.Fatal("corruption note must appear only for the name containing U+FFFD")
	}
	if !strings.Contains(body, `name="name"`) || !strings.Contains(body, `value="Usina Exempl�"`) {
		t.Fatal("name input must be prefilled with the effective name")
	}

	post := func(name string) *http.Response {
		t.Helper()
		form := url.Values{"csrf_token": {token}, "name": {name}, "timezone": {p.Timezone}}
		req, err := http.NewRequest("POST", fmt.Sprintf("%s/manage/plants/%d", ts.URL, p.ID), strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST status=%d, want 303", resp.StatusCode)
		}
		return resp
	}
	corrected := `Usina "Exemplo" <script>alert(1)</script> & teste`
	for _, name := range []string{corrected, "", "   "} {
		resp := post(name)
		if !strings.HasPrefix(resp.Header.Get("Location"), "/manage?success=") {
			t.Fatalf("save failed: %s", resp.Header.Get("Location"))
		}
		got, err := st.GetPlant(context.Background(), p.ID)
		if err != nil || got.Name != corrected {
			t.Fatalf("effective name=%q err=%v", got.Name, err)
		}
	}
	body = getBody(t, ts.URL+"/manage")
	if strings.Contains(body, note) || strings.Contains(body, corrected) {
		t.Fatal("corrected name must lose the note and must be HTML escaped")
	}
	if !strings.Contains(body, `value="Usina &#34;Exemplo&#34; &lt;script&gt;alert(1)&lt;/script&gt; &amp; teste"`) {
		t.Fatal("name input must escape quotes, markup and ampersands")
	}
	resp := post(strings.Repeat("á", 121))
	location := resp.Header.Get("Location")
	if !strings.HasPrefix(location, "/manage?error=") {
		t.Fatalf("long name must use the page error mechanism: %s", location)
	}
	errResp, err := client.Get(ts.URL + location)
	if err != nil {
		t.Fatal(err)
	}
	defer errResp.Body.Close()
	errBody, err := io.ReadAll(errResp.Body)
	if err != nil || !strings.Contains(string(errBody), "120 caracteres") {
		t.Fatalf("missing rendered name length error: %v", err)
	}
	got, err := st.GetPlant(context.Background(), p.ID)
	if err != nil || got.Name != corrected {
		t.Fatalf("rejected edit changed name=%q err=%v", got.Name, err)
	}
	if resp := post(strings.Repeat("á", 120)); !strings.HasPrefix(resp.Header.Get("Location"), "/manage?success=") {
		t.Fatal("120 Unicode characters must be accepted")
	}
}
