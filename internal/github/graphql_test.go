package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
)

func TestGraphQL(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	const query = `query($owner:String!){repository(owner:$owner,name:"repo"){nameWithOwner}}`

	var (
		mu            sync.Mutex
		gotTokenPath  string
		gotTokenBody  string
		gotGraphPath  string
		gotGraphAuth  string
		gotGraphQuery string
		gotGraphVars  map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/app/installations/11/access_tokens":
			mu.Lock()
			gotTokenPath, gotTokenBody = r.URL.Path, string(body)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"token":"ghs_inst","expires_at":%q}`, expires)
		case "/graphql":
			var payload struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			_ = json.Unmarshal(body, &payload)
			mu.Lock()
			gotGraphPath, gotGraphAuth = r.URL.Path, r.Header.Get("Authorization")
			gotGraphQuery, gotGraphVars = payload.Query, payload.Variables
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{"repository":{"nameWithOwner":"acme/repo"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := &ghapp.App{
		Creds:  ghapp.AppCredentials{AppID: 12345, PEM: testAppPEM(t)},
		APIURL: srv.URL,
	}
	var out struct {
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	}
	if err := a.GraphQL(context.Background(), 11, query, map[string]any{"owner": "acme"}, &out); err != nil {
		t.Fatalf("GraphQL() error = %v", err)
	}
	if out.Repository.NameWithOwner != "acme/repo" {
		t.Errorf("decoded data = %+v, want repository nameWithOwner acme/repo", out)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotTokenPath != "/app/installations/11/access_tokens" {
		t.Errorf("token path = %q, want /app/installations/11/access_tokens", gotTokenPath)
	}
	if strings.TrimSpace(gotTokenBody) != "{}" {
		t.Errorf("token body = %q, want {}", gotTokenBody)
	}
	if gotGraphPath != "/graphql" {
		t.Errorf("GraphQL path = %q, want /graphql", gotGraphPath)
	}
	if gotGraphAuth != "Bearer ghs_inst" {
		t.Errorf("GraphQL Authorization = %q, want Bearer ghs_inst", gotGraphAuth)
	}
	if gotGraphQuery != query {
		t.Errorf("GraphQL query = %q, want %q", gotGraphQuery, query)
	}
	if gotGraphVars["owner"] != "acme" {
		t.Errorf("GraphQL variables = %v, want owner=acme", gotGraphVars)
	}
}

func TestGraphQLReturnsErrors(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/11/access_tokens":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"token":"ghs_inst","expires_at":%q}`, expires)
		case "/graphql":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":null,"errors":[{"message":"boom"},{"message":"nope"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := &ghapp.App{
		Creds:  ghapp.AppCredentials{AppID: 12345, PEM: testAppPEM(t)},
		APIURL: srv.URL,
	}
	var out struct{}
	err := a.GraphQL(context.Background(), 11, "query", nil, &out)
	if err == nil {
		t.Fatal("GraphQL() error = nil, want the GraphQL errors")
	}
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %q, want both GraphQL messages", err)
	}
}
