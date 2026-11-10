package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/postgres"
)

type AdminCorrectionStore interface {
	Begin(context.Context) (domain.UnitOfWork, error)
	GetRequest(context.Context, string) (*domain.Request, error)
	GetPriceVersion(context.Context, string) (*postgres.PriceVersion, error)
	CorrectSettlement(context.Context, domain.UnitOfWork, postgres.CorrectionCommand) error
}

type AdminCorrectionsHandler struct {
	store AdminCorrectionStore
	audit management.AuditTxRecorder
	clock domain.Clock
}

func NewAdminCorrectionsHandler(store AdminCorrectionStore, audit management.AuditTxRecorder, clock domain.Clock) *AdminCorrectionsHandler {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &AdminCorrectionsHandler{store: store, audit: audit, clock: clock}
}

func (h *AdminCorrectionsHandler) Register(g *gin.RouterGroup) {
	g.POST("/model-requests/:id/corrections", h.Correct)
}

type correctionEvidenceRequest struct {
	AttemptID string `json:"attempt_id"`
	Buckets   struct {
		Input      *int64          `json:"input_tokens"`
		CacheRead  *int64          `json:"cache_read_tokens"`
		CacheWrite *int64          `json:"cache_write_tokens"`
		Output     *int64          `json:"output_tokens"`
		Reasoning  json.RawMessage `json:"reasoning_tokens"`
	} `json:"buckets"`
	Extras         map[string]*int64 `json:"extras"`
	EvidenceRef    string            `json:"evidence_ref"`
	Reason         string            `json:"reason"`
	IdempotencyKey string            `json:"idempotency_key"`
}

func (h *AdminCorrectionsHandler) Correct(c *gin.Context) {
	var body correctionEvidenceRequest
	if err := strictBindJSON(c, &body); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid correction evidence"))
		return
	}
	requestID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request ID"))
		return
	}
	attemptID, err := uuid.Parse(body.AttemptID)
	if err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid attempt ID"))
		return
	}
	body.Reason = strings.TrimSpace(body.Reason)
	body.EvidenceRef = strings.TrimSpace(body.EvidenceRef)
	body.IdempotencyKey = strings.TrimSpace(body.IdempotencyKey)
	if body.Reason == "" || body.EvidenceRef == "" || body.IdempotencyKey == "" {
		fail(c, domain.NewError(domain.CodeInvalidInput, "reason, evidence_ref and idempotency_key are required"))
		return
	}
	buckets := domain.UsageBuckets{
		InputTokens: body.Buckets.Input, CacheReadTokens: body.Buckets.CacheRead,
		CacheWriteTokens: body.Buckets.CacheWrite, OutputTokens: body.Buckets.Output,
	}
	if buckets.InputTokens == nil || buckets.CacheReadTokens == nil ||
		buckets.CacheWriteTokens == nil || buckets.OutputTokens == nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "all four normalized token buckets are required, including explicit zeros"))
		return
	}
	if len(body.Buckets.Reasoning) != 0 {
		if err := json.Unmarshal(body.Buckets.Reasoning, &buckets.ReasoningTokens); err != nil ||
			buckets.ReasoningTokens == nil {
			fail(c, domain.NewError(domain.CodeInvalidInput, "reasoning_tokens must be an integer when provided"))
			return
		}
		if *buckets.ReasoningTokens > *buckets.OutputTokens {
			fail(c, domain.NewError(domain.CodeInvalidInput, "reasoning_tokens must be included in output_tokens"))
			return
		}
	}
	tokens, err := access.NormalizedTokens(buckets)
	if err != nil {
		fail(c, err)
		return
	}
	ctx := c.Request.Context()
	req, err := h.store.GetRequest(ctx, requestID.String())
	if err != nil {
		fail(c, err)
		return
	}
	if req.PriceVersionID == nil {
		fail(c, domain.NewError(domain.CodeConflict, "request has no pinned price"))
		return
	}
	price, err := h.store.GetPriceVersion(ctx, *req.PriceVersionID)
	if err != nil {
		fail(c, err)
		return
	}
	validPrice := price.ModelID == req.ModelID
	switch req.ChargeSource {
	case domain.ChargeSourcePlan:
		validPrice = validPrice && price.Kind == postgres.PriceSaleCredit &&
			price.Unit == "microcredit" && price.Currency == ""
	case domain.ChargeSourceWallet:
		validPrice = validPrice && price.Kind == postgres.PriceSaleMoney &&
			price.Unit == "micromoney" && price.Currency != ""
	default:
		validPrice = false
	}
	if !validPrice {
		fail(c, domain.NewError(domain.CodeConflict, "request pinned price does not match its billing source"))
		return
	}
	pure, err := price.Pure()
	if err != nil {
		fail(c, err)
		return
	}
	extras := make(map[string]int64, len(body.Extras))
	for name, units := range body.Extras {
		if _, priced := pure.ExtraRates[name]; !priced || units == nil || *units < 0 {
			fail(c, domain.NewError(domain.CodeInvalidInput, "extras must contain only priced nonnegative integer evidence"))
			return
		}
		extras[name] = *units
	}
	// Omitted priced or admitted capabilities are unknown, not zero.
	for name := range pure.ExtraRates {
		if _, present := extras[name]; !present {
			fail(c, domain.NewError(domain.CodeInvalidInput, "explicit evidence is required for every priced extra"))
			return
		}
	}
	for name := range req.ExtraBounds {
		if _, present := extras[name]; !present {
			fail(c, domain.NewError(domain.CodeInvalidInput, "explicit evidence is required for every admitted extra"))
			return
		}
	}
	raw, err := json.Marshal(struct {
		SchemaVersion int              `json:"schema_version"`
		EvidenceRef   string           `json:"evidence_ref"`
		Extras        map[string]int64 `json:"extras"`
	}{1, body.EvidenceRef, extras})
	if err != nil {
		fail(c, err)
		return
	}
	usage := domain.UsageRecord{
		RequestID: req.ID, AttemptID: attemptID.String(), Source: domain.UsageReported,
		Buckets: buckets, RawUsage: domain.ExtensionConfig{SchemaVersion: 1, Raw: raw},
	}
	charge, err := pure.Quote(usage, extras)
	if err != nil {
		fail(c, domain.WrapError(domain.CodeInvalidInput, "correction evidence cannot be priced", err))
		return
	}
	amount := charge.Credit
	if charge.Money != nil {
		amount = domain.Microcredit(charge.Money.Micros)
	}
	op := OperatorOf(c)
	if h.audit == nil || op.UserID == "" || op.AppID == "" {
		fail(c, domain.NewError(domain.CodeInternal, "transactional operator audit is required"))
		return
	}
	uow, err := h.store.Begin(ctx)
	if err != nil {
		fail(c, err)
		return
	}
	defer uow.Rollback(ctx)
	if err := h.store.CorrectSettlement(ctx, uow, postgres.CorrectionCommand{
		RequestID: req.ID, Corrected: usage, CorrectedMicros: amount, CorrectedTokens: &tokens,
		Reason: body.Reason, OperatorSubject: "user:" + op.UserID + "@app:" + op.AppID,
		IdempotencyKey: body.IdempotencyKey, At: h.clock.Now(),
	}); err != nil {
		fail(c, err)
		return
	}
	if err := h.audit.RecordTx(ctx, uow, management.AuditEvent{
		Action: "settlement.correct", ObjectType: "model_request", ObjectID: req.ID,
		Reason: body.Reason, ActorUser: op.UserID, ActorApp: op.AppID,
		Detail: management.SanitizeDetail(map[string]any{
			"attempt_id": usage.AttemptID, "price_version_id": price.ID,
			"amount_micros": strconv.FormatInt(int64(amount), 10),
			"unit":          price.Unit, "currency": price.Currency,
			"evidence_ref": body.EvidenceRef, "idempotency_key": body.IdempotencyKey,
		}),
	}); err != nil {
		fail(c, domain.WrapError(domain.CodeInternal, "correction audit write failed", err))
		return
	}
	if err := uow.Commit(ctx); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": gin.H{
		"request_id": req.ID, "amount_micros": strconv.FormatInt(int64(amount), 10),
		"unit": price.Unit, "currency": price.Currency, "source": domain.UsageReported,
	}})
}
