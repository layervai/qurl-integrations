package slackdata

import (
	"context"
	"errors"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// auditClock is an advanceable clock for audit tests: each PutAuditEntry needs a
// distinct nanosecond so the time-ordered sort keys don't collide (a real human
// confirm click is always milliseconds apart).
type auditClock struct{ t time.Time }

func (c *auditClock) now() time.Time { return c.t }
func (c *auditClock) tick()          { c.t = c.t.Add(time.Second) }

func newAuditStore(c *auditClock, fake *agentFakeDDB) *AgentStore {
	return &AgentStore{Client: fake, TableName: "agent_state", Now: c.now}
}

func putAudit(t *testing.T, s *AgentStore, c *auditClock, partition string, e *AuditEntry) {
	t.Helper()
	if err := s.PutAuditEntry(context.Background(), partition, e); err != nil {
		t.Fatalf("PutAuditEntry(%s/%s): %v", e.Actor, e.Action, err)
	}
	c.tick()
}

// The defining contract: a list returns only the VIEWER's own actions, newest-first.
// Decoys for U12 (prefix-adjacent to U1) and U2 must not leak — the "#" delimiter
// after the user id stops the U1/U12 begins_with collision, which is the
// security-critical per-viewer boundary the whole surface rests on.
func TestAuditEntries_ListsUserOwnNewestFirst(t *testing.T) {
	c := &auditClock{t: time.Unix(1_700_000_000, 0)}
	fake := newAgentFakeDDB()
	s := newAuditStore(c, fake)

	putAudit(t, s, c, "T1", &AuditEntry{Actor: "U1", Action: "get", Target: "staging"})
	putAudit(t, s, c, "T1", &AuditEntry{Actor: "U12", Action: "revoke", Target: "decoy-other-user"})
	putAudit(t, s, c, "T1", &AuditEntry{Actor: "U1", Action: "revoke", Target: "analytics"})
	putAudit(t, s, c, "T1", &AuditEntry{Actor: "U2", Action: "get", Target: "decoy-u2"})
	putAudit(t, s, c, "T1", &AuditEntry{Actor: "U1", Action: "set_alias", Target: "dash"})

	got, err := s.ListAuditEntries(context.Background(), "T1", "U1", 10)
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want exactly U1's 3 own entries (no U12/U2 leak), got %d: %+v", len(got), got)
	}
	// Newest-first: set_alias (written last) -> revoke -> get (written first).
	for i, want := range []string{"set_alias", "revoke", "get"} {
		if got[i].Action != want {
			t.Fatalf("entry %d action=%q, want %q (newest-first)", i, got[i].Action, want)
		}
		if got[i].Actor != "U1" {
			t.Fatalf("entry %d actor=%q — per-viewer scope leaked another user", i, got[i].Actor)
		}
	}
	// The store stamps the write time onto the entry.
	if got[0].UnixSec != c.t.Add(-time.Second).Unix() {
		t.Fatalf("entry time not store-stamped: got %d", got[0].UnixSec)
	}
}

func TestAuditEntries_Limit(t *testing.T) {
	c := &auditClock{t: time.Unix(1_700_000_000, 0)}
	s := newAuditStore(c, newAgentFakeDDB())
	for range 5 {
		putAudit(t, s, c, "T1", &AuditEntry{Actor: "U1", Action: "get", Target: "r"})
	}
	got, err := s.ListAuditEntries(context.Background(), "T1", "U1", 2)
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit=2 should cap the result at 2, got %d", len(got))
	}
}

func TestAuditEntries_ReadTimeTTL(t *testing.T) {
	c := &auditClock{t: time.Unix(1_700_000_000, 0)}
	s := newAuditStore(c, newAgentFakeDDB())
	s.AuditTTL = time.Hour
	putAudit(t, s, c, "T1", &AuditEntry{Actor: "U1", Action: "get", Target: "r"})

	// Jump past the TTL window — the reaper lags, so the read-time filter is what
	// makes AuditTTL a real bound.
	c.t = c.t.Add(2 * time.Hour)
	got, err := s.ListAuditEntries(context.Background(), "T1", "U1", 10)
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a past-TTL entry must be filtered at read time, got %d", len(got))
	}
}

func TestAuditEntries_SkipsCorruptPayload(t *testing.T) {
	c := &auditClock{t: time.Unix(1_700_000_000, 0)}
	fake := newAgentFakeDDB()
	s := newAuditStore(c, fake)
	putAudit(t, s, c, "T1", &AuditEntry{Actor: "U1", Action: "get", Target: "good"})

	// A corrupt item under the same user prefix (and a higher sk, so newest-first it
	// sorts ahead) must be skipped, not fail the whole list.
	corruptSK := "audit#U1#9999999999999999999"
	fake.items["T1|"+corruptSK] = map[string]ddbtypes.AttributeValue{
		attrAgentPK:      stringAttr("T1"),
		attrAgentSK:      stringAttr(corruptSK),
		attrAuditPayload: stringAttr("{not valid json"),
		attrAgentTTL:     numberAttr(c.now().Add(time.Hour).Unix()),
	}

	got, err := s.ListAuditEntries(context.Background(), "T1", "U1", 10)
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(got) != 1 || got[0].Target != "good" {
		t.Fatalf("corrupt entry must be skipped, leaving the valid one, got %+v", got)
	}
}

func TestAuditEntries_QueryErrorSurfaces(t *testing.T) {
	fake := newAgentFakeDDB()
	fake.queryErr = errors.New("ddb unavailable")
	s := newAuditStore(&auditClock{t: time.Unix(1_700_000_000, 0)}, fake)
	if _, err := s.ListAuditEntries(context.Background(), "T1", "U1", 10); err == nil {
		t.Fatal("a Query error must surface from ListAuditEntries")
	}
}

func TestAuditEntries_Validation(t *testing.T) {
	s := newAuditStore(&auditClock{t: time.Unix(1_700_000_000, 0)}, newAgentFakeDDB())
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		partition string
		entry     AuditEntry
	}{
		{"empty partition", "", AuditEntry{Actor: "U1", Action: "get"}},
		{"empty actor", "T1", AuditEntry{Action: "get"}},
		{"empty action", "T1", AuditEntry{Actor: "U1"}},
	} {
		if err := s.PutAuditEntry(ctx, tc.partition, &tc.entry); err == nil {
			t.Fatalf("PutAuditEntry(%s): want error", tc.name)
		}
	}
	if _, err := s.ListAuditEntries(ctx, "", "U1", 10); err == nil {
		t.Fatal("ListAuditEntries with empty partition must error")
	}
	if _, err := s.ListAuditEntries(ctx, "T1", "", 10); err == nil {
		t.Fatal("ListAuditEntries with empty user must error")
	}
}
