package internal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/teams/internal/oauth"
	"github.com/layervai/qurl-integrations/apps/teams/internal/teamsdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

type ctxKey string

const (
	testResourcesPath       = "/v1/resources"
	testWorkspaceTableName  = "workspaces"
	testConversationChannel = "channel"
)

var errUnexpectedTestStoreCall = errors.New("unexpected test store call")

func TestHandleGetDMChecksPersonalConversationBeforeMint(t *testing.T) {
	var createCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == testResourcesPath:
			writeJSON(t, w, http.StatusOK, map[string]any{
				"data": []map[string]any{{
					"resource_id": "r_live",
					"slug":        "docs",
					"status":      client.StatusActive,
				}},
				"meta": map[string]any{"has_more": false},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/resources/r_live/qurls":
			createCalls++
			writeJSON(t, w, http.StatusCreated, map[string]any{
				"data": map[string]any{
					"resource_id": "r_live",
					"qurl_link":   "https://qurl.test/link",
				},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	store := newTestTeamsStore(&stubDDBClient{
		getItemFunc: func(_ context.Context, params *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			switch aws.ToString(params.TableName) {
			case "policies":
				if strings.Contains(aws.ToString(params.ProjectionExpression), "alias_bindings") {
					return &dynamodb.GetItemOutput{}, nil
				}
				return &dynamodb.GetItemOutput{
					Item: map[string]ddbtypes.AttributeValue{
						"allowed_resource_ids": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_live"}},
					},
				}, nil
			case testWorkspaceTableName:
				return &dynamodb.GetItemOutput{}, nil
			default:
				t.Fatalf("unexpected table: %s", aws.ToString(params.TableName))
				return nil, errUnexpectedTestStoreCall
			}
		},
	})

	h := NewHandler(&HandlerConfig{
		AdminStore: store,
		Messages:   &stubMessagePoster{},
	})
	qc := client.New(srv.URL, "test-key", client.WithRetry(0))
	activity := &Activity{
		ID:   "activity-1",
		From: ChannelAccount{ID: "user-1"},
	}
	scope := scopeInfo{TenantID: "tenant-1", ScopeID: "channel-1"}
	cmd := &Command{Resource: "docs", Flags: map[string]string{"dm": "true"}}

	_, err := h.handleGet(context.Background(), qc, scope, activity, cmd)
	if err == nil {
		t.Fatal("expected handleGet error")
	}
	if msg := teamsUserMessageForError(err); !strings.Contains(msg, "Open a personal chat with the bot once") {
		t.Fatalf("message = %q, want personal chat guidance", msg)
	}
	if createCalls != 0 {
		t.Fatalf("Create qURL calls = %d, want 0", createCalls)
	}
}

func TestHandleGetSetsAccessLimitsAndScopedIdempotencyKey(t *testing.T) {
	var (
		gotIdempotencyKey string
		gotBody           map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == testResourcesPath:
			writeJSON(t, w, http.StatusOK, map[string]any{
				"data": []map[string]any{{
					"resource_id": "r:live",
					"slug":        "docs",
					"status":      client.StatusActive,
				}},
				"meta": map[string]any{"has_more": false},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/resources/r:live/qurls":
			gotIdempotencyKey = r.Header.Get("Idempotency-Key")
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			writeJSON(t, w, http.StatusCreated, map[string]any{
				"data": map[string]any{
					"resource_id": "r:live",
					"qurl_link":   "https://qurl.test/link",
				},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	store := newTestTeamsStore(&stubDDBClient{
		getItemFunc: func(_ context.Context, params *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			switch aws.ToString(params.TableName) {
			case "policies":
				if strings.Contains(aws.ToString(params.ProjectionExpression), "alias_bindings") {
					return &dynamodb.GetItemOutput{}, nil
				}
				return &dynamodb.GetItemOutput{
					Item: map[string]ddbtypes.AttributeValue{
						"allowed_resource_ids": &ddbtypes.AttributeValueMemberSS{Value: []string{"r:live"}},
					},
				}, nil
			case testWorkspaceTableName:
				return &dynamodb.GetItemOutput{}, nil
			default:
				t.Fatalf("unexpected table: %s", aws.ToString(params.TableName))
				return nil, errUnexpectedTestStoreCall
			}
		},
	})

	h := NewHandler(&HandlerConfig{
		AdminStore: store,
		Messages:   &stubMessagePoster{},
	})
	qc := client.New(srv.URL, "test-key", client.WithRetry(0))
	activity := &Activity{
		ID:   "activity:1",
		From: ChannelAccount{ID: "user:1"},
	}
	scope := scopeInfo{TenantID: "tenant:1", ScopeID: "channel:1"}
	cmd := &Command{Resource: "docs", Flags: map[string]string{"reason": "test reason"}}

	reply, err := h.handleGet(context.Background(), qc, scope, activity, cmd)
	if err != nil {
		t.Fatalf("handleGet error = %v", err)
	}
	if !strings.Contains(reply, "https://qurl.test/link") {
		t.Fatalf("reply = %q, want minted qURL link", reply)
	}
	if got, _ := gotBody["expires_in"].(string); got != teamsGetResourceLinkExpiry {
		t.Fatalf("expires_in = %q, want %q", got, teamsGetResourceLinkExpiry)
	}
	if got, _ := gotBody["session_duration"].(string); got != teamsGetResourceSessionDuration {
		t.Fatalf("session_duration = %q, want %q", got, teamsGetResourceSessionDuration)
	}
	if got, _ := gotBody["one_time_use"].(bool); !got {
		t.Fatalf("one_time_use = %v, want true", gotBody["one_time_use"])
	}
	if got, _ := gotBody["max_sessions"].(float64); int(got) != teamsGetResourceMaxSessions {
		t.Fatalf("max_sessions = %v, want %d", gotBody["max_sessions"], teamsGetResourceMaxSessions)
	}
	wantIdempotencyKey := getQURLIdempotencyKey("tenant:1", "channel:1", "user:1", "r:live", activity)
	if gotIdempotencyKey != wantIdempotencyKey {
		t.Fatalf("Idempotency-Key = %q, want %q", gotIdempotencyKey, wantIdempotencyKey)
	}
}

func TestHandleProtectConnectorRevokesBootstrapKeyWhenDMFails(t *testing.T) {
	var revokeCalls int
	var gotIdempotencyKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == testResourcesPath:
			writeJSON(t, w, http.StatusCreated, map[string]any{
				"data": map[string]any{
					"resource_id": "r_connector",
					"slug":        "prod-dashboard",
					"type":        client.ResourceTypeTunnel,
					"status":      client.StatusActive,
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/api-keys":
			gotIdempotencyKey = r.Header.Get("Idempotency-Key")
			writeJSON(t, w, http.StatusCreated, map[string]any{
				"data": map[string]any{
					"key_id":  "k_bootstrap",
					"api_key": "bootstrap-secret",
				},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/api-keys/k_bootstrap":
			revokeCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	refJSON, err := json.Marshal(&teamsdata.PersonalConversationRef{
		ServiceURL:     "https://service.example.test",
		ConversationID: "conv-personal",
	})
	if err != nil {
		t.Fatalf("marshal ref: %v", err)
	}
	store := newTestTeamsStore(&stubDDBClient{
		getItemFunc: func(_ context.Context, params *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			switch aws.ToString(params.TableName) {
			case testWorkspaceTableName:
				if strings.Contains(aws.ToString(params.ProjectionExpression), "personal_conversation_refs") {
					return &dynamodb.GetItemOutput{
						Item: map[string]ddbtypes.AttributeValue{
							"personal_conversation_refs": &ddbtypes.AttributeValueMemberM{
								Value: map[string]ddbtypes.AttributeValue{
									"user-1": &ddbtypes.AttributeValueMemberS{Value: string(refJSON)},
								},
							},
						},
					}, nil
				}
				return &dynamodb.GetItemOutput{
					Item: map[string]ddbtypes.AttributeValue{
						"owner_id": &ddbtypes.AttributeValueMemberS{Value: "user-1"},
					},
				}, nil
			default:
				t.Fatalf("unexpected table: %s", aws.ToString(params.TableName))
				return nil, errUnexpectedTestStoreCall
			}
		},
		updateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			expr := aws.ToString(params.UpdateExpression)
			switch {
			case strings.Contains(expr, "allowed_resource_ids"):
				return &dynamodb.UpdateItemOutput{}, nil
			case strings.Contains(expr, "SET alias_bindings = :empty_map"):
				// Seed write that makes the nested alias path writable.
				return &dynamodb.UpdateItemOutput{}, nil
			case strings.Contains(expr, "REMOVE alias_bindings.#alias"):
				return nil, &ddbtypes.ConditionalCheckFailedException{}
			case strings.Contains(expr, "alias_bindings.#alias = :rid"):
				return &dynamodb.UpdateItemOutput{}, nil
			default:
				t.Fatalf("unexpected update expression: %s", expr)
				return nil, errUnexpectedTestStoreCall
			}
		},
	})

	messages := &stubMessagePoster{
		sendTextFunc: func(context.Context, string, string, string) error {
			return errors.New("dm delivery failed")
		},
	}
	h := NewHandler(&HandlerConfig{
		AdminStore: store,
		Messages:   messages,
	})
	qc := client.New(srv.URL, "test-key", client.WithRetry(0))

	_, err = h.handleProtectConnector(context.Background(), qc, scopeInfo{
		TenantID: "tenant-1",
		ScopeID:  "channel-1",
	}, &Activity{
		ID:   "activity-1",
		From: ChannelAccount{ID: "user-1"},
	}, []string{"prod-dashboard"})
	if err == nil {
		t.Fatal("expected handleProtectConnector error")
	}
	if msg := teamsUserMessageForError(err); !strings.Contains(msg, "temporary key was revoked") {
		t.Fatalf("message = %q, want revoked-key guidance", msg)
	}
	if revokeCalls != 1 {
		t.Fatalf("bootstrap key revoke calls = %d, want 1", revokeCalls)
	}
	wantIdempotencyKey := tunnelBootstrapIdempotencyKey("tenant-1", "channel-1", "user-1", "prod-dashboard", "activity:activity-1")
	if gotIdempotencyKey != wantIdempotencyKey {
		t.Fatalf("Idempotency-Key = %q, want %q", gotIdempotencyKey, wantIdempotencyKey)
	}
}

func TestListAllResourcesExcludesRevoked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != testResourcesPath {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"data": []map[string]any{
				{
					"resource_id": "r_live",
					"slug":        "live-db",
					"status":      client.StatusActive,
				},
				{
					"resource_id": "r_revoked",
					"slug":        "dead-db",
					"status":      client.StatusRevoked,
				},
			},
			"meta": map[string]any{"has_more": false},
		})
	}))
	defer srv.Close()

	resources, err := listAllResources(context.Background(), client.New(srv.URL, "test-key", client.WithRetry(0)))
	if err != nil {
		t.Fatalf("listAllResources error = %v", err)
	}
	if len(resources) != 1 || resources[0].ResourceID != "r_live" {
		t.Fatalf("resources = %#v, want only r_live", resources)
	}
}

func TestProcessMessageSanitizesFeedbackFailure(t *testing.T) {
	messages := &stubMessagePoster{}
	h := NewHandler(&HandlerConfig{
		Messages: messages,
		Feedback: stubFeedbackPoster{err: errors.New("feedback webhook returned 500: internal endpoint failed")},
	})
	activity := &Activity{
		Text: "feedback this broke",
		From: ChannelAccount{ID: "user-1"},
		Conversation: ConversationAccount{
			ID:               "conv-1",
			ConversationType: "personal",
			TenantID:         "tenant-1",
		},
	}

	if err := h.processMessage(context.Background(), activity); err != nil {
		t.Fatalf("processMessage error = %v", err)
	}
	if len(messages.replies) != 1 {
		t.Fatalf("reply count = %d, want 1", len(messages.replies))
	}
	if got := messages.replies[0]; got != "Feedback couldn't be delivered right now. Try again later." {
		t.Fatalf("reply = %q", got)
	}
}

func TestHandleSetupRotateUsesRequestContext(t *testing.T) {
	const key ctxKey = "request_scope"
	var gotValue any
	store := newTestTeamsStore(&stubDDBClient{
		getItemFunc: func(ctx context.Context, _ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			gotValue = ctx.Value(key)
			return &dynamodb.GetItemOutput{}, nil
		},
	})
	h := NewHandler(&HandlerConfig{
		AdminStore: store,
		Setup: oauth.SetupConfig{
			StateSecret:  []byte(strings.Repeat("x", oauth.StateMinSecret)),
			TeamsBaseURL: "https://teams.example.test",
		},
		OAuthEnabled: true,
	})
	ctx := context.WithValue(context.Background(), key, "present")

	reply, err := h.handleSetup(ctx, scopeInfo{TenantID: "tenant-1"}, &Activity{From: ChannelAccount{ID: "user-1"}}, &Command{
		Email:     "owner@example.test",
		SetupMode: SetupModeRotate,
	})
	if err != nil {
		t.Fatalf("handleSetup error = %v", err)
	}
	if gotValue != "present" {
		t.Fatalf("ListAdmins context value = %v, want propagated request context", gotValue)
	}
	if !strings.Contains(reply, "https://teams.example.test") {
		t.Fatalf("reply = %q, want setup link", reply)
	}
}

func TestProcessMessageWithoutAdminStoreFailsClosed(t *testing.T) {
	messages := &stubMessagePoster{}
	h := NewHandler(&HandlerConfig{Messages: messages})
	activity := &Activity{
		Text: "list",
		From: ChannelAccount{ID: "user-1"},
		Conversation: ConversationAccount{
			ID:               "conv-1",
			ConversationType: testConversationChannel,
			TenantID:         "tenant-1",
		},
	}

	if err := h.processMessage(context.Background(), activity); err != nil {
		t.Fatalf("processMessage error = %v", err)
	}
	if len(messages.replies) != 1 {
		t.Fatalf("reply count = %d, want 1", len(messages.replies))
	}
	if got := messages.replies[0]; got != "Teams admin features are not configured on this deployment." {
		t.Fatalf("reply = %q", got)
	}
}

func TestHandleActivityAcknowledgesMessageBeforeReplyCompletes(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	finished := make(chan struct{})
	messages := &stubMessagePoster{
		replyFunc: func(context.Context, *Activity, string) error {
			close(started)
			<-release
			close(finished)
			return nil
		},
	}
	h := NewHandler(&HandlerConfig{Messages: messages})
	activity := &Activity{
		Type: "message",
		Text: "help",
		From: ChannelAccount{ID: "user-1"},
		Conversation: ConversationAccount{
			ID:               "conv-1",
			ConversationType: testConversationChannel,
			TenantID:         "tenant-1",
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/teams/messages", http.NoBody)
	rec := httptest.NewRecorder()

	go func() {
		h.handleActivity(rec, req, activity)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("handleActivity did not acknowledge promptly")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("async message processing did not start")
	}
	if drained := h.WaitTimeout(50 * time.Millisecond); drained {
		t.Fatal("WaitTimeout returned true with async worker still blocked")
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("async message processing did not finish")
	}
	if drained := h.WaitTimeout(time.Second); !drained {
		t.Fatal("WaitTimeout returned false after async worker completed")
	}
}

type stubMessagePoster struct {
	replies      []string
	replyFunc    func(ctx context.Context, in *Activity, text string) error
	sendTextFunc func(ctx context.Context, serviceURL, conversationID, text string) error
}

func (s *stubMessagePoster) Reply(ctx context.Context, in *Activity, text string) error {
	if s.replyFunc != nil {
		return s.replyFunc(ctx, in, text)
	}
	s.replies = append(s.replies, text)
	return nil
}

func (s *stubMessagePoster) SendText(ctx context.Context, serviceURL, conversationID, text string) error {
	if s.sendTextFunc != nil {
		return s.sendTextFunc(ctx, serviceURL, conversationID, text)
	}
	return nil
}

type stubFeedbackPoster struct {
	err error
}

func (s stubFeedbackPoster) Post(context.Context, string, string, string) error {
	return s.err
}

type stubDDBClient struct {
	getItemFunc    func(ctx context.Context, params *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error)
	putItemFunc    func(ctx context.Context, params *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error)
	updateItemFunc func(ctx context.Context, params *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error)
	deleteItemFunc func(ctx context.Context, params *dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error)
	queryFunc      func(ctx context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error)
}

func (s *stubDDBClient) GetItem(ctx context.Context, params *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if s.getItemFunc != nil {
		return s.getItemFunc(ctx, params)
	}
	return &dynamodb.GetItemOutput{}, nil
}

func (s *stubDDBClient) PutItem(ctx context.Context, params *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	if s.putItemFunc != nil {
		return s.putItemFunc(ctx, params)
	}
	return &dynamodb.PutItemOutput{}, nil
}

func (s *stubDDBClient) UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if s.updateItemFunc != nil {
		return s.updateItemFunc(ctx, params)
	}
	return &dynamodb.UpdateItemOutput{}, nil
}

func (s *stubDDBClient) DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	if s.deleteItemFunc != nil {
		return s.deleteItemFunc(ctx, params)
	}
	return &dynamodb.DeleteItemOutput{}, nil
}

func (s *stubDDBClient) Query(ctx context.Context, params *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	if s.queryFunc != nil {
		return s.queryFunc(ctx, params)
	}
	return &dynamodb.QueryOutput{}, nil
}

func newTestTeamsStore(ddbClient teamsdata.DynamoDBClient) *teamsdata.Store {
	return &teamsdata.Store{
		Client:                ddbClient,
		WorkspaceMappingsName: testWorkspaceTableName,
		ChannelPoliciesName:   "policies",
		Now: func() time.Time {
			return time.Unix(1_700_000_000, 0).UTC()
		},
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("encode payload: %v", err)
	}
}
