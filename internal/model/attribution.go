package model

import "encoding/json"

// attributionFieldMaxLen caps every string sub-field of an attribution
// touch. The website-side tracker can hand us arbitrarily long URLs and
// campaign strings; 200 runes is generous for any real UTM/referrer value
// and keeps the orders.attribution JSONB column bounded.
const attributionFieldMaxLen = 200

// AttributionTouch is one touchpoint snapshot (first_touch / last_touch)
// in the order attribution payload. Every field is optional; captured_at
// is kept as the raw string the tracker sent — it is never parsed into a
// time.Time.
type AttributionTouch struct {
	UtmSource      *string `json:"utm_source,omitempty"`
	UtmMedium      *string `json:"utm_medium,omitempty"`
	UtmCampaign    *string `json:"utm_campaign,omitempty"`
	UtmContent     *string `json:"utm_content,omitempty"`
	ReferrerDomain *string `json:"referrer_domain,omitempty"`
	LandingPath    *string `json:"landing_path,omitempty"`
	CapturedAt     *string `json:"captured_at,omitempty"`
}

// Attribution is the optional attribution object accepted on
// POST /payments/orders and persisted verbatim (post-sanitize) onto the
// order row. Unknown keys in the input are dropped by struct decoding.
type Attribution struct {
	FirstTouch *AttributionTouch `json:"first_touch,omitempty"`
	LastTouch  *AttributionTouch `json:"last_touch,omitempty"`
}

// SanitizeAttribution decodes the raw request payload, truncates every
// non-nil string sub-field to attributionFieldMaxLen runes, and re-marshals
// the result for verbatim persistence. Nil/empty input and a payload with
// no touches at all ({} or both touches null) return nil so the column
// stores SQL NULL instead of an empty object. Invalid JSON returns an
// error; the caller maps that to a 400.
func SanitizeAttribution(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var a Attribution
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if a.FirstTouch == nil && a.LastTouch == nil {
		return nil, nil
	}
	a.FirstTouch.truncate()
	a.LastTouch.truncate()
	out, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (t *AttributionTouch) truncate() {
	if t == nil {
		return
	}
	for _, p := range []*string{
		t.UtmSource, t.UtmMedium, t.UtmCampaign, t.UtmContent,
		t.ReferrerDomain, t.LandingPath, t.CapturedAt,
	} {
		if p == nil {
			continue
		}
		if r := []rune(*p); len(r) > attributionFieldMaxLen {
			*p = string(r[:attributionFieldMaxLen])
		}
	}
}
