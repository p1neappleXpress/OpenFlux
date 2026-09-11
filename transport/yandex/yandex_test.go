package yandex

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func clientConfigPage(config string) string {
	return `<!DOCTYPE html><html><head><script id="client-config" type="application/json">` +
		config + `</script></head><body></body></html>`
}

func serveConfig(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// A "volga" client-config carries no balancer_url; the request must fail with an
// error instead of panicking.
func TestFetchDocInfoVolgaConfigReturnsError(t *testing.T) {
	config := `{"officeActionData":{"officeType":"volga","editor_config":{"type":"desktop",` +
		`"document":{"key":"volgaDocKey","fileType":"docx","url":"http://localhost:12701/disk/x",` +
		`"title":"x.docx"},"token":"volga-jwt"}}}`

	url := serveConfig(t, clientConfigPage(config))

	info, err := (&YandexDocsTransport{}).fetchDocInfo(url, "0000000001")
	if err == nil {
		t.Fatalf("expected an error for a config without balancer_url, got info %+v", info)
	}
	if !strings.Contains(err.Error(), "balancer_url") {
		t.Errorf("error should name the missing field, got: %v", err)
	}
}

func TestFetchDocInfoMissingOfficeActionDataReturnsError(t *testing.T) {
	url := serveConfig(t, clientConfigPage(`{"somethingElse":true}`))

	if _, err := (&YandexDocsTransport{}).fetchDocInfo(url, "0000000001"); err == nil {
		t.Fatal("expected an error when officeActionData is absent")
	}
}

func TestFetchDocInfoMissingDocumentReturnsError(t *testing.T) {
	config := `{"officeActionData":{"balancer_url":"https://balancer.example.net",` +
		`"editor_config":{"type":"desktop","token":"jwt"}}}`

	url := serveConfig(t, clientConfigPage(config))

	if _, err := (&YandexDocsTransport{}).fetchDocInfo(url, "0000000001"); err == nil {
		t.Fatal("expected an error when editor_config.document is absent")
	}
}

func TestFetchDocInfoMissingTokenReturnsError(t *testing.T) {
	config := `{"officeActionData":{"balancer_url":"https://balancer.example.net",` +
		`"editor_config":{"type":"desktop","document":{"key":"docKey"}}}}`

	url := serveConfig(t, clientConfigPage(config))

	if _, err := (&YandexDocsTransport{}).fetchDocInfo(url, "0000000001"); err == nil {
		t.Fatal("expected an error when editor_config.token is absent")
	}
}

func TestFetchDocInfoInvalidJSONReturnsError(t *testing.T) {
	url := serveConfig(t, clientConfigPage(`{not valid json`))

	if _, err := (&YandexDocsTransport{}).fetchDocInfo(url, "0000000001"); err == nil {
		t.Fatal("expected an error when client-config is not valid JSON")
	}
}

func TestFetchDocInfoMissingConfigReturnsError(t *testing.T) {
	url := serveConfig(t, `<!DOCTYPE html><html><body>no config here</body></html>`)

	if _, err := (&YandexDocsTransport{}).fetchDocInfo(url, "0000000001"); err == nil {
		t.Fatal("expected an error when client-config is absent")
	}
}

func TestFetchDocInfoValidConfig(t *testing.T) {
	config := `{"officeActionData":{"balancer_url":"https://balancer.example.net",` +
		`"editor_config":{"type":"desktop","token":"jwt-token",` +
		`"document":{"key":"docKey123","fileType":"docx","url":"http://localhost:12701/disk/x",` +
		`"title":"x.docx","permissions":{"edit":false,"download":true}}}}}`

	url := serveConfig(t, clientConfigPage(config))

	info, err := (&YandexDocsTransport{}).fetchDocInfo(url, "0000000001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Token != "jwt-token" {
		t.Errorf("Token = %q, want %q", info.Token, "jwt-token")
	}
	if info.DocID != "docKey123" {
		t.Errorf("DocID = %q, want %q", info.DocID, "docKey123")
	}
	if info.Host != "balancer.example.net" {
		t.Errorf("Host = %q, want %q", info.Host, "balancer.example.net")
	}
	if info.Origin != "https://balancer.example.net" {
		t.Errorf("Origin = %q, want %q", info.Origin, "https://balancer.example.net")
	}
	wantWs := "wss://balancer.example.net/2024.1.1-375/doc/docKey123/c/?EIO=4&transport=websocket"
	if info.WsURL != wantWs {
		t.Errorf("WsURL = %q, want %q", info.WsURL, wantWs)
	}
	if info.Permissions["download"] != true {
		t.Errorf("Permissions = %+v, want download=true", info.Permissions)
	}
	if got := info.OpenCmd["id"]; got != "docKey123" {
		t.Errorf("OpenCmd[id] = %v, want %q", got, "docKey123")
	}
	if got := info.OpenCmd["userid"]; got != "0000000001" {
		t.Errorf("OpenCmd[userid] = %v, want %q", got, "0000000001")
	}
}

// An absent permissions object must not fail the request; it falls back to an
// empty map.
func TestFetchDocInfoMissingPermissionsFallsBack(t *testing.T) {
	config := `{"officeActionData":{"balancer_url":"https://balancer.example.net",` +
		`"editor_config":{"type":"desktop","token":"jwt-token",` +
		`"document":{"key":"docKey123","url":"http://localhost:12701/disk/x"}}}}`

	url := serveConfig(t, clientConfigPage(config))

	info, err := (&YandexDocsTransport{}).fetchDocInfo(url, "0000000001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Permissions == nil {
		t.Error("Permissions should fall back to an empty map, got nil")
	}
}
