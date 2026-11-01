package internal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNonAgentGetConfirmedGroupDMRefusesBeforeMint(t *testing.T) {
	for _, surface := range []string{"slash", "button"} {
		t.Run(surface, func(t *testing.T) {
			const channelID = "G_group_dm"
			ts := newAdminTestServers(t)
			ts.seedPolicySet(t, testAdminTeamID, channelID, "prod-db", []string{testResourceIDFix})
			var minted, classified atomic.Int32
			ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
				minted.Add(1)
				writeCreateFixture(t, w, "https://qurl.link/fixture", testResourceIDFix)
			})
			h := newAdminTestHandler(t, ts)
			h.cfg.ResolveConversationInfo = func(ctx context.Context, teamID, enterpriseID, id string) (ConversationInfo, error) {
				classified.Add(1)
				if teamID != testAdminTeamID || id != channelID {
					t.Error("wrong conversation authority")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Error("unbounded conversation classification")
				}
				return ConversationInfo{IsMPIM: true}, nil
			}
			inv := newAdminSlashInvokerOnChannel(t, h, channelID)
			var reply string
			if surface == "slash" {
				_, _, reply = inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
			} else {
				body := listCreateQurlBlockActionsBody(t, testAdminTeamID, testAdminUserID, channelID, inv.responseU.URL, listCreateQurlActionID, "prod-db")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
				if w.Code != http.StatusOK {
					t.Fatal("button ack failed", w.Code)
				}
				reply = parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
			}
			h.Wait()
			if classified.Load() != 1 || minted.Load() != 0 || !strings.Contains(reply, agentConfirmGetUnsupportedSurfaceReply) || strings.Contains(reply, "https://qurl.link/") {
				t.Fatalf("confirmed mpim was not refused before mint: classified=%d minted=%d reply=%q", classified.Load(), minted.Load(), reply)
			}
		})
	}
}

func TestNonAgentGetKeepsPrivateChannelsUnknownMetadataAndDMs(t *testing.T) {
	for _, surface := range []string{"slash", "button"} {
		for _, kind := range []string{"private", "unavailable", "missing-resolver", "channel", "dm"} {
			t.Run(surface+"/"+kind, func(t *testing.T) {
				channelID := "G_private"
				if kind == "channel" {
					channelID = "C_channel"
				}
				if kind == "dm" {
					channelID = "D_direct"
				}
				ts := newAdminTestServers(t)
				ts.seedPolicySet(t, testAdminTeamID, channelID, "prod-db", []string{testResourceIDFix})
				var minted, classified atomic.Int32
				ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
					minted.Add(1)
					writeCreateFixture(t, w, "https://qurl.link/fixture", testResourceIDFix)
				})
				h := newAdminTestHandler(t, ts)
				if kind != "missing-resolver" {
					h.cfg.ResolveConversationInfo = func(context.Context, string, string, string) (ConversationInfo, error) {
						classified.Add(1)
						if kind == "channel" || kind == "dm" {
							t.Error("ordinary channel or DM needed conversation lookup")
						}
						if kind == "unavailable" {
							return ConversationInfo{}, errors.New("missing_scope")
						}
						return ConversationInfo{}, nil
					}
				}
				inv := newAdminSlashInvokerOnChannel(t, h, channelID)
				var reply string
				if surface == "slash" {
					_, _, reply = inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
				} else {
					body := listCreateQurlBlockActionsBody(t, testAdminTeamID, testAdminUserID, channelID, inv.responseU.URL, listCreateQurlActionID, "prod-db")
					w := httptest.NewRecorder()
					h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
					if w.Code != http.StatusOK {
						t.Fatal("button ack failed")
					}
					reply = parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
				}
				h.Wait()
				expectedClassified := int32(0)
				if kind == "private" || kind == "unavailable" {
					expectedClassified = 1
				}
				if minted.Load() != 1 || classified.Load() != expectedClassified || !strings.Contains(reply, "https://qurl.link/fixture") {
					t.Fatalf("existing delivery regressed: mint=%d classify=%d reply=%q", minted.Load(), classified.Load(), reply)
				}
			})
		}
	}
}

func TestNonAgentGetExplicitDMDoesNotNeedGroupEphemeralDelivery(t *testing.T) {
	const channelID = "G_group_dm"
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, channelID, "prod-db", []string{testResourceIDFix})
	var minted, delivered atomic.Int32
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		minted.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/fixture", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	h.cfg.ResolveConversationInfo = func(context.Context, string, string, string) (ConversationInfo, error) {
		t.Error("explicit DM consulted group ephemeral classification")
		return ConversationInfo{IsMPIM: true}, nil
	}
	h.cfg.PostDMBlocks = func(_ context.Context, teamID, enterpriseID, userID string, _ []any, fallbackText string) error {
		delivered.Add(1)
		if teamID != testAdminTeamID || userID != testAdminUserID || !strings.Contains(fallbackText, "https://qurl.link/fixture") {
			t.Error("wrong private recipient or missing link")
		}
		return nil
	}
	inv := newAdminSlashInvokerOnChannel(t, h, channelID)
	_, _, reply := inv.invokeAdminAsync("get $prod-db dm:true", testAdminTeamID, testAdminUserID)
	h.Wait()
	if minted.Load() != 1 || delivered.Load() != 1 || strings.Contains(reply, "https://qurl.link/") {
		t.Fatalf("explicit DM failed or leaked in source conversation: minted=%d delivered=%d reply=%q", minted.Load(), delivered.Load(), reply)
	}
}
