package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientMethodsAndErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/botSECRET_TOKEN/getMe":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"id":                          999,
					"is_bot":                      true,
					"first_name":                  "CorpBot",
					"username":                    "corp_mcp_bot",
					"can_read_all_group_messages": true,
				},
			})
		case "/botSECRET_TOKEN/getChatMember":
			var body map[string]int64
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["user_id"] == 404 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok":          false,
					"error_code":  400,
					"description": "Bad Request: user not found",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"status":    "restricted",
					"is_member": false,
					"user":      map[string]any{"id": body["user_id"], "first_name": "Ex"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client, err := NewClient("SECRET_TOKEN", "", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	me, err := client.GetMe(context.Background())
	if err != nil {
		t.Fatalf("getMe: %v", err)
	}
	if me.ID != 999 || !me.CanReadAllGroupMessages {
		t.Fatalf("unexpected getMe result: %+v", me)
	}

	cm, err := client.GetChatMember(context.Background(), -1001, 50)
	if err != nil {
		t.Fatalf("getChatMember: %v", err)
	}
	if cm.Status != "restricted" || cm.EffectiveIsMember() {
		t.Fatalf("expected restricted non-member, got %+v", cm)
	}

	_, err = client.GetChatMember(context.Background(), -1001, 404)
	if !IsNotParticipantError(err) {
		t.Fatalf("expected IsNotParticipantError, got %v", err)
	}
}

func TestClientRedactsTokenOnNetworkError(t *testing.T) {
	client, err := NewClient("SUPER_SECRET_BOT_TOKEN", "", WithBaseURL("http://127.0.0.1:1"))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = client.GetMe(context.Background())
	if err == nil {
		t.Fatal("expected network error")
	}
	if strings.Contains(err.Error(), "SUPER_SECRET_BOT_TOKEN") {
		t.Fatalf("bot token leaked in error: %v", err)
	}
}
