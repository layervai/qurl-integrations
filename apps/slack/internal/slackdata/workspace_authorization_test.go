package slackdata

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func TestValidatePolicyAuthorization(t *testing.T) {
	for _, mode := range []string{"read only", "current", "wrong store", "wrong team", "malformed", "missing generation", "missing installation", "missing binding", "revoked", "reinstalled", "read failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			item := policyTestWorkspace()
			reads := 0
			store := newStore(&stubDDB{getItemFn: func(in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				reads++
				if !aws.ToBool(in.ConsistentRead) || aws.ToString(in.TableName) != "ws" || readString(in.Key, attrSlackTeamID) != "T1" {
					t.Fatal("scope validation must strongly read the captured workspace")
				}
				if mode == "read failure" {
					return nil, errors.New("local unavailable")
				}
				return &dynamodb.GetItemOutput{Item: item}, nil
			}})
			authorization := PolicyAuthorization{store: store, teamID: "T1", userID: testCallerSlackID, ownerID: testOwnerSlackID, generation: "original-generation", installation: "original-installation"}
			team := "T1"
			wantReads, wantStatus := 1, http.StatusConflict
			switch mode {
			case "read only":
				wantReads, wantStatus = 0, 0
			case "current":
				wantStatus = 0
			case "wrong store":
				store = nil
				wantReads = 0
			case "wrong team":
				team = "T2"
				wantReads = 0
			case "malformed":
				ctx = context.WithValue(ctx, policyAuthorizationContextKey{}, "invalid")
				wantReads = 0
			case "missing generation":
				authorization.generation = ""
				wantReads = 0
			case "missing installation":
				authorization.installation = ""
				wantReads = 0
			case "missing binding":
				clear(item)
			case "revoked":
				delete(item, attrAdminSlackUserIDs)
			case "reinstalled":
				item[attrPolicyInstallation] = stringAttr("replacement-installation")
			case "read failure":
				wantStatus = http.StatusServiceUnavailable
			}
			if mode != "read only" && mode != "malformed" {
				ctx = authorization.Context(ctx)
			}
			err := ValidatePolicyAuthorization(ctx, store, team)
			if wantStatus == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				assertPolicyStatus(t, err, wantStatus)
			}
			if reads != wantReads {
				t.Fatalf("workspace reads=%d want=%d", reads, wantReads)
			}
		})
	}
}
