# Google Maps Support Plan

## Overview

Add Google Maps URL detection and protection to the QURL Discord bot. Users can DM a Google Maps link to the bot, which extracts location data and stores it as a protected QURL resource.

## Design Decision: Option B (Coordinates Only)

Store only coordinates/place query in a JSON payload, NOT the full embed URL. The viewer will inject its own Google Maps API key server-side. This avoids leaking API keys in stored data.

### Stored payload format

```json
{
  "type": "google-map",
  "query": "Seattle, WA",
  "lat": 47.6062,
  "lng": -122.3321
}
```

## Supported URL Formats

| Format | Example | Extracted |
|--------|---------|-----------|
| Place | `google.com/maps/place/Seattle,WA` | query |
| Place + coords | `google.com/maps/place/Seattle,+WA/@47.6,-122.3,15z` | query + lat/lng |
| Coordinate | `google.com/maps/@47.6062,-122.3321,15z` | lat/lng |
| Search | `google.com/maps/search/pizza+near+seattle` | query |
| Embed | `google.com/maps/embed/v1/place?key=...&q=Seattle,WA` | query |
| Short links | `goo.gl/maps/...`, `maps.app.goo.gl/...` | detected only (not parsed) |

## Phases

### Phase 1 (Current)

- URL detection and parsing (`services/maps_parser.py`)
- Integration into DM message handler
- Upload map metadata as JSON via existing `upload_file` flow
- Config flag: `GOOGLE_MAPS_ENABLED`

### Phase 2 (Future)

- Short link resolution (follow redirects to get canonical URL)
- Viewer-side map rendering with server-injected API key
- Map preview in Discord embed (static map image)

### Phase 3 (Future)

- Directions/route support
- Street View support
- Custom map styles

## API Key Security (Required Before Launch)

### Google Cloud Console (manual, 5 minutes)
1. Go to https://console.cloud.google.com/apis/credentials
2. Create or select the API key used for Maps Embed API
3. Under "Application restrictions": set HTTP referrers to `https://fileviewer.layerv.ai/*`
4. Under "API restrictions": restrict to "Maps Embed API" only
5. Under "Quotas" (APIs & Services -> Maps Embed API -> Quotas):
   set daily limit to 1,000 loads/day

### AWS CloudWatch Alarm (add to Terraform)
Add a CloudWatch alarm for map-resource views:
- Metric: custom counter `MapResourceViews` emitted by the viewer
- Threshold: sum > 500 in 1 hour
- Action: SNS notification to ops team

## Kill Switch

The `GOOGLE_MAPS_ENABLED` env var (config.py) disables all Maps URL handling
when set to `false`. For ECS deployments: update the env var in the ECS task
definition and force a new deployment. This is the kill switch until SSM
Parameter Store runtime reads are implemented.

## Deployment Verification Protocol (Required Before Enabling)

**Do NOT set `GOOGLE_MAPS_ENABLED=true` in any environment until this protocol is completed.**

### Steps
1. Confirm `GOOGLE_MAPS_ENABLED=false` in ECS task definition (Terraform default)
2. Upload a test map payload manually:
   ```bash
   curl -X POST https://getqurllink.layerv.xyz/api/upload \
     -F "file=@/dev/stdin;filename=map.json;type=application/json" \
     <<< '{"type":"google-map","query":"Seattle,WA","lat":47.6,"lng":-122.3}'
   ```
3. Retrieve the resource via the returned qurl_link in a browser
4. **Verify the recipient experience:**
   - If the recipient sees a rendered map → Phase 2 viewer is deployed, safe to enable
   - If the recipient sees raw JSON text → Phase 2 NOT deployed, do NOT enable
   - If the upload returns 4xx → upload service rejects JSON, do NOT enable
5. Record the result in a comment on the PR that enables the flag
6. Only then: set `GOOGLE_MAPS_ENABLED=true` in Terraform and deploy

### Why this matters
If `GOOGLE_MAPS_ENABLED=true` is set before Phase 2 (viewer rendering) is deployed,
recipients will see raw JSON containing coordinates in plaintext — worse than the
problem we're solving. The kill switch prevents accidental exposure, but flipping it
requires this verification.
