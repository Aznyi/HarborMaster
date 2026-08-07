package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// The notification endpoint tests.
//
// Almost every one is NEGATIVE, because the properties that matter here are
// negative: the credential never comes back, an operator cannot create a
// destination, an anonymous caller cannot do anything, and a malformed
// identifier is indistinguishable from an absent one.

// The webhook URL used throughout. Its path is the credential, which is the
// whole point: every test below asserts this string does not appear in a
// response.
const testWebhookURL = "https://hooks.example.test/services/T000/B000/SECRETPATHVALUE"

var (
	testDestinationID = "ndst_" + strings.Repeat("a", 20)
	testRuleID        = "nrul_" + strings.Repeat("b", 20)
	testDeliveryID    = "ndlv_" + strings.Repeat("c", 20)
)

// ------------------------------------------------------------------ doubles --

// fakeNotificationAdmin records what it was asked to do.
type fakeNotificationAdmin struct {
	available bool

	destination domain.NotificationDestination
	rule        domain.NotificationRule

	// createdSecret is what the handler passed inward. The tests assert the
	// URL reached the service, which is the other half of "it never comes
	// back": a credential that was silently dropped would also never appear in
	// a response.
	createdSecret domain.NotificationSecret
	updatedChange store.DestinationChange
	tested        string
	archived      string

	err error
}

func (f *fakeNotificationAdmin) Available() bool { return f.available }

func (f *fakeNotificationAdmin) CreateDestination(
	_ context.Context,
	destination domain.NotificationDestination,
	secret domain.NotificationSecret,
	_ service.Actor,
) (service.DestinationResult, error) {
	if f.err != nil {
		return service.DestinationResult{}, f.err
	}
	f.createdSecret = secret
	destination.DestinationID = testDestinationID
	// What the repository does: the safe rendering is DERIVED, and the
	// credential is stored somewhere the read paths cannot reach.
	destination.Endpoint = "https://hooks.example.test"
	f.destination = destination
	return service.DestinationResult{Destination: destination}, nil
}

func (f *fakeNotificationAdmin) UpdateDestination(
	_ context.Context,
	_ string,
	change store.DestinationChange,
	_ service.Actor,
) (service.DestinationResult, error) {
	if f.err != nil {
		return service.DestinationResult{}, f.err
	}
	f.updatedChange = change
	return service.DestinationResult{Destination: f.destination}, nil
}

func (f *fakeNotificationAdmin) ArchiveDestination(
	_ context.Context, destinationID string, _ service.Actor,
) error {
	f.archived = destinationID
	return f.err
}

func (f *fakeNotificationAdmin) Destination(
	context.Context, string,
) (domain.NotificationDestination, error) {
	if f.err != nil {
		return domain.NotificationDestination{}, f.err
	}
	return f.destination, nil
}

func (f *fakeNotificationAdmin) Destinations(
	context.Context, bool, store.Page,
) ([]domain.NotificationDestination, int, error) {
	if f.err != nil {
		return nil, 0, f.err
	}
	return []domain.NotificationDestination{f.destination}, 1, nil
}

func (f *fakeNotificationAdmin) TestDestination(
	_ context.Context, destinationID string, _ service.Actor,
) error {
	f.tested = destinationID
	return f.err
}

func (f *fakeNotificationAdmin) CreateRule(
	_ context.Context, rule domain.NotificationRule, _ service.Actor,
) (service.RuleResult, error) {
	if f.err != nil {
		return service.RuleResult{}, f.err
	}
	rule.RuleID = testRuleID
	f.rule = rule
	return service.RuleResult{Rule: rule}, nil
}

func (f *fakeNotificationAdmin) UpdateRule(
	context.Context, string, store.NotificationRuleChange, service.Actor,
) (service.RuleResult, error) {
	if f.err != nil {
		return service.RuleResult{}, f.err
	}
	return service.RuleResult{Rule: f.rule}, nil
}

func (f *fakeNotificationAdmin) ArchiveRule(context.Context, string, service.Actor) error {
	return f.err
}

func (f *fakeNotificationAdmin) Rule(context.Context, string) (domain.NotificationRule, error) {
	if f.err != nil {
		return domain.NotificationRule{}, f.err
	}
	return f.rule, nil
}

func (f *fakeNotificationAdmin) Rules(
	context.Context, bool, store.Page,
) ([]domain.NotificationRule, int, error) {
	if f.err != nil {
		return nil, 0, f.err
	}
	return []domain.NotificationRule{f.rule}, 1, nil
}

// fakeNotificationReader serves the delivery history.
type fakeNotificationReader struct {
	enabled  bool
	readable bool
	delivery domain.NotificationDelivery
	filter   store.DeliveryFilter
	err      error
}

func (f *fakeNotificationReader) Enabled() bool  { return f.enabled }
func (f *fakeNotificationReader) Readable() bool { return f.readable }

func (f *fakeNotificationReader) Deliveries(
	_ context.Context, filter store.DeliveryFilter,
) ([]domain.NotificationDelivery, int, error) {
	if f.err != nil {
		return nil, 0, f.err
	}
	f.filter = filter
	return []domain.NotificationDelivery{f.delivery}, 1, nil
}

func (f *fakeNotificationReader) Delivery(
	context.Context, string,
) (domain.NotificationDelivery, error) {
	if f.err != nil {
		return domain.NotificationDelivery{}, f.err
	}
	return f.delivery, nil
}

func (f *fakeNotificationReader) Summary(context.Context) (domain.NotificationSummary, error) {
	if f.err != nil {
		return domain.NotificationSummary{}, f.err
	}
	return domain.NotificationSummary{Enabled: f.enabled, Destinations: 1, Rules: 1}, nil
}

// ------------------------------------------------------------------ harness --

func liveNotifications() (*fakeNotificationAdmin, *fakeNotificationReader) {
	admin := &fakeNotificationAdmin{
		available: true,
		destination: domain.NotificationDestination{
			DestinationID: testDestinationID,
			Name:          "operations chat",
			Channel:       domain.ChannelSlack,
			Enabled:       true,
			Endpoint:      "https://hooks.example.test",
		},
		rule: domain.NotificationRule{
			RuleID:          testRuleID,
			Name:            "things that went wrong",
			Enabled:         true,
			MinimumSeverity: domain.NotifyWarning,
			Destinations:    []string{testDestinationID},
		},
	}
	reader := &fakeNotificationReader{
		enabled:  true,
		readable: true,
		delivery: domain.NotificationDelivery{
			DeliveryID:      testDeliveryID,
			DestinationID:   testDestinationID,
			DestinationName: "operations chat",
			Channel:         domain.ChannelSlack,
			Event:           domain.EventExecutionFailed,
			Severity:        domain.NotifyCritical,
			Title:           "web could not be updated",
			Result:          domain.DeliveryFailed,
			Attempts:        3,
			QueuedAt:        time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC),
		},
	}
	return admin, reader
}

func newNotificationServer(
	t *testing.T,
	admin NotificationAdmin,
	reader NotificationReader,
) *Server {
	t.Helper()
	return newAuthedServer(notificationOptions(admin, reader))
}

func notificationOptions(admin NotificationAdmin, reader NotificationReader) Options {
	return Options{
		Health:            &fakeHealth{},
		NotificationAdmin: admin,
		Notifications:     reader,
		Logger:            discardLogger(),
		Config:            config.Server{MaxRequestBytes: 8192},
		Now:               func() time.Time { return time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC) },
		Assets:            testAssets(),
	}
}

// -------------------------------------------------------------------- tests --

// No response in the notification API contains a credential.
//
// The single most important property here. A Slack, Discord, or Teams webhook
// URL is a bearer token in the shape of a path: anyone who reads one can post
// into that channel forever.
func TestNoNotificationResponseCarriesACredential(t *testing.T) {
	t.Parallel()

	admin, reader := liveNotifications()
	srv := newNotificationServer(t, admin, reader)

	// Create one WITH a credential, then read it back every way the API allows.
	created := doJSON(t, srv, http.MethodPost,
		APIPrefix+"/notifications/destinations",
		`{"name":"chat","channel":"slack","url":"`+testWebhookURL+`"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %s", created.Code, created.Body.String())
	}
	// The credential DID reach the service. A handler that silently dropped it
	// would also pass the leak check below, and would be a worse bug.
	if admin.createdSecret.URL != testWebhookURL {
		t.Fatalf("the URL did not reach the service: %q", admin.createdSecret.URL)
	}

	responses := []string{created.Body.String()}
	for _, path := range []string{
		APIPrefix + "/notifications",
		APIPrefix + "/notifications/destinations",
		APIPrefix + "/notifications/destinations/" + testDestinationID,
		APIPrefix + "/notifications/rules",
		APIPrefix + "/notifications/rules/" + testRuleID,
		APIPrefix + "/notifications/deliveries",
		APIPrefix + "/notifications/deliveries/" + testDeliveryID,
	} {
		rec := do(t, srv, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s returned %d: %s", path, rec.Code, rec.Body.String())
		}
		responses = append(responses, rec.Body.String())
	}

	for _, body := range responses {
		for _, secret := range []string{testWebhookURL, "SECRETPATHVALUE", "/services/T000"} {
			if strings.Contains(body, secret) {
				t.Fatalf("a notification response carries %q:\n%s", secret, body)
			}
		}
	}
}

// An operator may read the delivery history and may not configure anything.
//
// The split the user-facing permission model promises: "was anybody told about
// this" is an operator's question, and "where does this host send data" is an
// administrator's decision.
func TestAnOperatorMayReadTheHistoryAndNotConfigureIt(t *testing.T) {
	t.Parallel()

	admin, reader := liveNotifications()
	srv, _, _ := asRole(notificationOptions(admin, reader), domain.RoleOperator)

	for _, path := range []string{
		APIPrefix + "/notifications",
		APIPrefix + "/notifications/destinations",
		APIPrefix + "/notifications/rules",
		APIPrefix + "/notifications/deliveries",
	} {
		if rec := do(t, srv, http.MethodGet, path, nil); rec.Code != http.StatusOK {
			t.Errorf("an operator reading %s got %d, want 200", path, rec.Code)
		}
	}

	writes := []struct {
		method, path, body string
	}{
		{http.MethodPost, APIPrefix + "/notifications/destinations",
			`{"name":"x","channel":"slack","url":"` + testWebhookURL + `"}`},
		{http.MethodPatch, APIPrefix + "/notifications/destinations/" + testDestinationID,
			`{"name":"x"}`},
		{http.MethodDelete, APIPrefix + "/notifications/destinations/" + testDestinationID, ""},
		{http.MethodPost, APIPrefix + "/notifications/destinations/" + testDestinationID + "/test", ""},
		{http.MethodPost, APIPrefix + "/notifications/rules",
			`{"name":"x","destinations":["` + testDestinationID + `"]}`},
		{http.MethodPatch, APIPrefix + "/notifications/rules/" + testRuleID, `{"name":"x"}`},
		{http.MethodDelete, APIPrefix + "/notifications/rules/" + testRuleID, ""},
	}
	for _, write := range writes {
		rec := doJSON(t, srv, write.method, write.path, write.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("an operator calling %s %s got %d, want 403",
				write.method, write.path, rec.Code)
		}
	}

	// And nothing reached the service.
	if admin.tested != "" || admin.archived != "" || admin.createdSecret.URL != "" {
		t.Fatal("an operator's refused write still reached the service")
	}
}

// A viewer may read the history and configure nothing.
func TestAViewerMayReadTheHistory(t *testing.T) {
	t.Parallel()

	admin, reader := liveNotifications()
	srv, _, _ := asRole(notificationOptions(admin, reader), domain.RoleViewer)

	if rec := do(t, srv, http.MethodGet, APIPrefix+"/notifications/deliveries", nil); rec.Code != http.StatusOK {
		t.Errorf("a viewer reading the delivery history got %d, want 200", rec.Code)
	}
	rec := doJSON(t, srv, http.MethodPost,
		APIPrefix+"/notifications/destinations",
		`{"name":"x","channel":"slack","url":"`+testWebhookURL+`"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a viewer creating a destination got %d, want 403", rec.Code)
	}
}

// Every notification write is refused without a CSRF token.
//
// The routes accept a credential in a JSON body, which is exactly the shape of
// request a cross-site form cannot make -- but the token is what makes that
// true rather than incidental.
func TestNotificationWritesRequireACSRFToken(t *testing.T) {
	t.Parallel()

	admin, reader := liveNotifications()
	srv := newNotificationServer(t, admin, reader)

	for _, write := range []struct{ method, path string }{
		{http.MethodPost, APIPrefix + "/notifications/destinations"},
		{http.MethodPatch, APIPrefix + "/notifications/destinations/" + testDestinationID},
		{http.MethodDelete, APIPrefix + "/notifications/destinations/" + testDestinationID},
		{http.MethodPost, APIPrefix + "/notifications/destinations/" + testDestinationID + "/test"},
		{http.MethodPost, APIPrefix + "/notifications/rules"},
		{http.MethodPatch, APIPrefix + "/notifications/rules/" + testRuleID},
		{http.MethodDelete, APIPrefix + "/notifications/rules/" + testRuleID},
	} {
		request := httptest.NewRequest(write.method, write.path, strings.NewReader("{}"))
		request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: testSessionToken})
		// Deliberately no CSRF header.
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, request)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s without a CSRF token got %d, want 403",
				write.method, write.path, rec.Code)
		}
	}
}

// An anonymous caller reaches nothing.
func TestNotificationEndpointsRefuseAnonymousCallers(t *testing.T) {
	t.Parallel()

	admin, reader := liveNotifications()
	srv := newNotificationServer(t, admin, reader)

	for _, path := range []string{
		APIPrefix + "/notifications",
		APIPrefix + "/notifications/destinations",
		APIPrefix + "/notifications/rules",
		APIPrefix + "/notifications/deliveries",
		APIPrefix + "/notifications/deliveries/" + testDeliveryID,
	} {
		request := anonymous(httptest.NewRequest(http.MethodGet, path, nil))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, request)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("an anonymous GET %s got %d, want 401", path, rec.Code)
		}
	}
}

// A malformed identifier is indistinguishable from an absent one.
//
// Anything else would make the endpoint an oracle for which identifiers exist.
func TestAMalformedNotificationIdentifierIsANotFound(t *testing.T) {
	t.Parallel()

	admin, reader := liveNotifications()
	srv := newNotificationServer(t, admin, reader)

	for _, hostile := range []string{
		"..%2f..%2fetc%2fpasswd",
		"ndst_",
		"ndst_ZZZZZZZZZZZZZZZZZZZZ",
		"ndst_" + strings.Repeat("a", 100),
		"1'%20OR%20'1'='1",
	} {
		for _, shape := range []string{
			APIPrefix + "/notifications/destinations/" + hostile,
			APIPrefix + "/notifications/rules/" + hostile,
			APIPrefix + "/notifications/deliveries/" + hostile,
		} {
			rec := do(t, srv, http.MethodGet, shape, nil)
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET %s got %d, want 404", shape, rec.Code)
			}
			if strings.Contains(rec.Body.String(), hostile) {
				t.Errorf("GET %s echoed the caller's value back:\n%s", shape, rec.Body.String())
			}
		}
	}
}

// A destination's channel cannot be edited.
//
// Changing it would leave a stored credential of the wrong shape for what the
// destination now is, and every validation that would catch that ran when the
// credential was written.
func TestADestinationsChannelCannotBeEdited(t *testing.T) {
	t.Parallel()

	admin, reader := liveNotifications()
	srv := newNotificationServer(t, admin, reader)

	rec := doJSON(t, srv, http.MethodPatch,
		APIPrefix+"/notifications/destinations/"+testDestinationID,
		`{"channel":"email"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("editing the channel got %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if admin.updatedChange.Name != nil || admin.updatedChange.Secret != nil {
		t.Fatal("the refused edit still reached the service")
	}
}

// An edit that does not mention the URL carries no credential inward.
//
// Which is what makes "omitting it keeps the stored one" true rather than
// hoped for: the change the repository receives has a nil Secret.
func TestAnEditWithoutAURLCarriesNoCredential(t *testing.T) {
	t.Parallel()

	admin, reader := liveNotifications()
	srv := newNotificationServer(t, admin, reader)

	rec := doJSON(t, srv, http.MethodPatch,
		APIPrefix+"/notifications/destinations/"+testDestinationID,
		`{"name":"renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit returned %d: %s", rec.Code, rec.Body.String())
	}
	if admin.updatedChange.Secret != nil {
		t.Fatal("an edit that did not mention the URL still carried a credential")
	}
	if admin.updatedChange.Name == nil || *admin.updatedChange.Name != "renamed" {
		t.Fatal("the edit did not carry the field it did mention")
	}
}

// The test send takes no target from the caller beyond an identifier
// HarborMaster issued.
func TestATestSendCarriesNoCallerSuppliedDestination(t *testing.T) {
	t.Parallel()

	admin, reader := liveNotifications()
	srv := newNotificationServer(t, admin, reader)

	// A body naming somewhere else is accepted and ignored: there is nowhere in
	// the request type to put a URL.
	rec := doJSON(t, srv, http.MethodPost,
		APIPrefix+"/notifications/destinations/"+testDestinationID+"/test",
		`{"url":"https://attacker.example/","host":"169.254.169.254"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("test send returned %d: %s", rec.Code, rec.Body.String())
	}
	if admin.tested != testDestinationID {
		t.Fatalf("the service was asked to test %q, want the path's identifier", admin.tested)
	}
}

// A delivery filter's vocabularies are allowlisted.
func TestADeliveryFilterRefusesAValueOutsideItsVocabulary(t *testing.T) {
	t.Parallel()

	admin, reader := liveNotifications()
	srv := newNotificationServer(t, admin, reader)

	for _, query := range []string{
		"?result=%27%20OR%201%3D1%20--",
		"?result=exploded",
		"?event=drop%20table",
		"?event=%27%3B%20DROP%20TABLE%20notification_deliveries%3B%20--",
		"?destinationId=../../etc/passwd",
		"?failed=maybe",
	} {
		rec := do(t, srv, http.MethodGet, APIPrefix+"/notifications/deliveries"+query, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET /notifications/deliveries%s got %d, want 400", query, rec.Code)
		}
	}

	// And a legitimate one reaches the repository as a bound value.
	rec := do(t, srv, http.MethodGet,
		APIPrefix+"/notifications/deliveries?result=failed,succeeded&failed=true", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("a valid filter got %d: %s", rec.Code, rec.Body.String())
	}
	if len(reader.filter.Results) != 2 || !reader.filter.FailedOnly {
		t.Fatalf("the filter did not reach the repository: %+v", reader.filter)
	}
}

// A deployment with no notification store serves 503 rather than pretending.
func TestNotificationEndpointsReportBeingUnconfigured(t *testing.T) {
	t.Parallel()

	srv := newNotificationServer(t, nil, nil)

	for _, path := range []string{
		APIPrefix + "/notifications",
		APIPrefix + "/notifications/destinations",
		APIPrefix + "/notifications/rules",
		APIPrefix + "/notifications/deliveries",
	} {
		rec := do(t, srv, http.MethodGet, path, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s on an unconfigured deployment got %d, want 503", path, rec.Code)
		}
	}
}

// Sending switched off still serves the configuration and the history.
//
// An administrator sets destinations up and reviews past deliveries BEFORE
// turning delivery on, which is the order those should happen in.
func TestConfigurationStaysReadableWhileSendingIsOff(t *testing.T) {
	t.Parallel()

	admin, reader := liveNotifications()
	reader.enabled = false
	srv := newNotificationServer(t, admin, reader)

	rec := do(t, srv, http.MethodGet, APIPrefix+"/notifications", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status returned %d, want 200", rec.Code)
	}

	var status notificationStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Enabled {
		t.Fatal("the status claims delivery is on when it is off")
	}
	// And the vocabularies are served, so a rule editor is built from the same
	// source of truth the sender uses.
	if len(status.Channels) == 0 || len(status.Events) == 0 || len(status.Severities) == 0 {
		t.Fatalf("the status served no vocabularies: %+v", status)
	}

	if rec := do(t, srv, http.MethodGet, APIPrefix+"/notifications/destinations", nil); rec.Code != http.StatusOK {
		t.Errorf("destinations returned %d while sending is off, want 200", rec.Code)
	}
}

// An oversized body is refused before it is decoded.
func TestANotificationBodyIsBounded(t *testing.T) {
	t.Parallel()

	admin, reader := liveNotifications()
	srv := newNotificationServer(t, admin, reader)

	body := `{"name":"` + strings.Repeat("a", 32<<10) + `","channel":"slack"}`
	rec := doJSON(t, srv, http.MethodPost,
		APIPrefix+"/notifications/destinations",
		body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized body got %d, want 413", rec.Code)
	}
	if admin.createdSecret.URL != "" {
		t.Fatal("the oversized body still reached the service")
	}
}

// An unknown field is refused, so a typo does not silently do nothing.
func TestANotificationBodyRefusesUnknownFields(t *testing.T) {
	t.Parallel()

	admin, reader := liveNotifications()
	srv := newNotificationServer(t, admin, reader)

	rec := doJSON(t, srv, http.MethodPost,
		APIPrefix+"/notifications/destinations",
		`{"name":"x","channel":"slack","containerId":"deadbeef"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown field got %d, want 400: %s", rec.Code, rec.Body.String())
	}
}
