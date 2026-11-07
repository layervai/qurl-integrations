package qurlapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestStripeSetupURLAcceptsOnlyTheStripeDashboard(t *testing.T) {
	for value, want := range map[string]bool{
		"":                                       true,
		"https://dashboard.stripe.com/":          true,
		"https://dashboard.stripe.com/account":   true,
		"http://dashboard.stripe.com/":           false,
		"https://user@dashboard.stripe.com/":     false,
		"https://dashboard.stripe.com.evil.test": false,
		"https://connect.stripe.com/setup/x":     false,
		"https://dashboard.stripe.com:443/":      false,
	} {
		if got := stripeSetupURL(value); got != want {
			t.Errorf("stripeSetupURL(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestPublisherStripeStatusRefusesAnIncompleteOrWrongAnswer(t *testing.T) {
	valid := map[string]any{"account_id": "acct_publisher", "layerv_fee_percent": 3, "charges_enabled": true, "payouts_enabled": true, "renewing_subscriptions": 1}
	for name, change := range map[string]func(map[string]any){
		"older service":   func(d map[string]any) { delete(d, "charges_enabled") },
		"negative count":  func(d map[string]any) { d["renewing_subscriptions"] = -1 },
		"bad account":     func(d map[string]any) { d["account_id"] = "acct_bad/route" },
		"foreign setup":   func(d map[string]any) { d["setup_url"] = "https://attacker.test/" },
		"wrong data type": func(d map[string]any) { d["charges_enabled"] = "yes" },
	} {
		t.Run(name, func(t *testing.T) {
			data := map[string]any{}
			for k, v := range valid {
				data[k] = v
			}
			change(data)
			body, _ := json.Marshal(map[string]any{"data": data})
			c := &client{}
			if _, err := c.decodeStripeStatus(&restReply{status: http.StatusOK, body: body}); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	body, _ := json.Marshal(map[string]any{"data": valid})
	if _, err := (&client{}).decodeStripeStatus(&restReply{status: http.StatusOK, body: body}); err != nil {
		t.Fatalf("valid answer refused: %v", err)
	}
	if _, err := (&client{}).decodeStripeStatus(&restReply{status: http.StatusOK, body: []byte(`{"data":null}`)}); err == nil {
		t.Fatal("null data accepted")
	}
}
