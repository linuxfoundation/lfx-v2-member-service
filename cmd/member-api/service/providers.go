// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package service contains the Goa service implementation and provider
// initialization for the member API.
package service

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	sf "github.com/k-capehart/go-salesforce/v3"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/auth"
	infrab2borg "github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/b2borg"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/nats"
	infraproject "github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/project"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/salesforce"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/salesforce/pubsub"
	usecaseSvc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

var (
	natsClient *nats.NATSClient
	natsDoOnce sync.Once

	sfClient *sf.Salesforce
	sfDoOnce sync.Once

	sObjectClient *salesforce.SObjectClient
	sObjectDoOnce sync.Once

	projectResolver port.ProjectResolver
	resolverDoOnce  sync.Once

	// mockSettings is the shared in-memory settings store used in mock mode.
	// Reader and writer must point at the same instance so writes are visible to reads.
	mockSettings     *mock.MockB2BOrgSettings
	mockSettingsOnce sync.Once

	// mockWorkspaces is the shared in-memory workspace store used in mock mode.
	mockWorkspaces     *mock.MockOrgWorkspaces
	mockWorkspacesOnce sync.Once

	// apiSubs holds the drain callbacks for all NATS subscriptions registered by
	// QueueSubscriptions. Stored as func() error so the raw nats package does not
	// need to be imported here; each element is the Drain method of a *nats.Subscription.
	apiSubs []func() error
)

// natsTimeoutFromEnv reads the NATS_TIMEOUT environment variable and returns it
// as a time.Duration. Defaults to 10 seconds if unset. Calls log.Fatalf on an
// unparseable value.
func natsTimeoutFromEnv() time.Duration {
	raw := os.Getenv("NATS_TIMEOUT")
	if raw == "" {
		raw = "10s"
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Fatalf("invalid NATS timeout duration %q: %v", raw, err)
	}
	return d
}

func natsInit(ctx context.Context) {
	natsDoOnce.Do(func() {
		natsURL := os.Getenv("NATS_URL")
		if natsURL == "" {
			natsURL = "nats://localhost:4222"
		}

		natsMaxReconnect := os.Getenv("NATS_MAX_RECONNECT")
		if natsMaxReconnect == "" {
			natsMaxReconnect = "3"
		}
		natsMaxReconnectInt, err := strconv.Atoi(natsMaxReconnect)
		if err != nil {
			log.Fatalf("invalid NATS max reconnect value %s: %v", natsMaxReconnect, err)
		}

		natsReconnectWait := os.Getenv("NATS_RECONNECT_WAIT")
		if natsReconnectWait == "" {
			natsReconnectWait = "2s"
		}
		natsReconnectWaitDuration, err := time.ParseDuration(natsReconnectWait)
		if err != nil {
			log.Fatalf("invalid NATS reconnect wait duration %s: %v", natsReconnectWait, err)
		}

		config := nats.Config{
			URL:           natsURL,
			Timeout:       natsTimeoutFromEnv(),
			MaxReconnect:  natsMaxReconnectInt,
			ReconnectWait: natsReconnectWaitDuration,
		}

		client, err := nats.NewClient(ctx, config)
		if err != nil {
			log.Fatalf("failed to create NATS client: %v", err)
		}
		natsClient = client
	})
}

func sfInit(ctx context.Context) {
	sfDoOnce.Do(func() {
		cfg, err := salesforce.ConfigFromEnv()
		if err != nil {
			log.Fatalf("failed to read Salesforce config from environment: %v", err)
		}

		client, err := cfg.Init()
		if err != nil {
			log.Fatalf("failed to authenticate with Salesforce: %v", err)
		}
		sfClient = client
		slog.InfoContext(ctx, "Salesforce client initialised")
	})
}

func sObjectClientInit(ctx context.Context) {
	sObjectDoOnce.Do(func() {
		natsInit(ctx)
		sfInit(ctx)
		sObjectCache := nats.NewSObjectCache(natsClient)
		sObjectClient = salesforce.NewSObjectClient(sfClient, sObjectCache)
		slog.InfoContext(ctx, "Salesforce sObject client initialised")
	})
}

// CloseNATSClient closes the NATS client connection if it was initialised.
func CloseNATSClient() {
	if natsClient != nil {
		natsClient.Close() //nolint:errcheck // NATS Close does not return a meaningful error in practice.
	}
}

// NATSClientImpl returns the shared NATSClient singleton, initialising it if
// necessary. This is intended for use by main.go to register NATS RPC
// subscriptions after MemberReaderImpl has been called.
func NATSClientImpl(ctx context.Context) *nats.NATSClient {
	natsInit(ctx)
	return natsClient
}

// ProjectResolverImpl returns the shared ProjectResolver singleton, initialising
// all dependencies (NATS, Salesforce) as needed. Returns nil when
// REPOSITORY_SOURCE=mock — callers must guard on nil before use.
func ProjectResolverImpl(ctx context.Context) port.ProjectResolver {
	repoSource := os.Getenv("REPOSITORY_SOURCE")
	if repoSource == "" {
		repoSource = "salesforce"
	}

	switch repoSource {
	case "mock":
		return nil

	case "salesforce":
		resolverDoOnce.Do(func() {
			natsInit(ctx)
			sfInit(ctx)

			cache := nats.NewStorage(natsClient)
			projectRepo := salesforce.NewProjectRepo(sfClient)
			projectRPC := nats.NewProjectRPC(natsClient.Conn(), natsTimeoutFromEnv())
			projectResolver = infraproject.NewProjectResolver(projectRPC, projectRepo, cache)
		})
		return projectResolver

	default:
		log.Fatalf("unsupported REPOSITORY_SOURCE value: %q", repoSource)
		return nil
	}
}

// KeyContactWriterImpl initialises and returns the port.KeyContactWriter
// implementation selected by the REPOSITORY_SOURCE environment variable:
//
//   - "salesforce" (default) — Salesforce-backed writer with NATS KV cache invalidation.
//   - "mock"                 — Stub that returns NotImplemented for all writes; for local development without SF credentials.
func KeyContactWriterImpl(ctx context.Context) port.KeyContactWriter {
	repoSource := os.Getenv("REPOSITORY_SOURCE")
	if repoSource == "" {
		repoSource = "salesforce"
	}

	switch repoSource {
	case "mock":
		slog.InfoContext(ctx, "initialising mock key contact writer")
		return mock.NewMockKeyContactWriter()

	case "salesforce":
		slog.InfoContext(ctx, "initialising Salesforce key contact writer with conditional writes")
		sObjectClientInit(ctx)
		cache := nats.NewStorage(natsClient)
		contactRepo := salesforce.NewContactRepo(sfClient)
		contactsRepo := salesforce.NewKeyContactRepo(sfClient)
		return salesforce.NewKeyContactWriter(sfClient, sObjectClient, contactsRepo, contactRepo, cache)

	default:
		log.Fatalf("unsupported REPOSITORY_SOURCE value: %q", repoSource)
		return nil
	}
}

// MemberReaderImpl initialises and returns the port.MemberReader implementation
// selected by the REPOSITORY_SOURCE environment variable:
//
//   - "salesforce" (default) — Salesforce SOQL queries with NATS KV caching.
//   - "mock"                 — In-memory mock, for local development only.
func MemberReaderImpl(ctx context.Context) port.MemberReader {
	repoSource := os.Getenv("REPOSITORY_SOURCE")
	if repoSource == "" {
		repoSource = "salesforce"
	}

	switch repoSource {
	case "mock":
		slog.InfoContext(ctx, "initialising mock member reader")
		return mock.NewMockMembershipRepository()

	case "salesforce":
		slog.InfoContext(ctx, "initialising Salesforce member reader with NATS KV cache")

		natsInit(ctx)
		sfInit(ctx)

		// ProjectResolverImpl shares the singleton resolver so that the KV
		// cache entries written by the resolver during list/get calls are also
		// available to the NATS RPC handler registered in main.go.
		resolver := ProjectResolverImpl(ctx)
		cache := nats.NewStorage(natsClient)

		sObjectClientInit(ctx)
		return salesforce.NewMemberReader(
			salesforce.NewMemberRepo(sfClient),
			salesforce.NewMembershipRepo(sfClient),
			salesforce.NewKeyContactRepo(sfClient),
			salesforce.NewKeyContactReader(sObjectClient),
			resolver,
			cache,
		)

	default:
		log.Fatalf("unsupported REPOSITORY_SOURCE value: %q", repoSource)
		return nil
	}
}

// B2BOrgReaderImpl initialises and returns the port.B2BOrgReader implementation
// selected by the REPOSITORY_SOURCE environment variable:
//
//   - "salesforce" (default) — Salesforce SOQL queries with NATS KV caching.
//   - "mock"                 — In-memory mock that always returns empty pages.
func B2BOrgReaderImpl(ctx context.Context) port.B2BOrgReader {
	repoSource := os.Getenv("REPOSITORY_SOURCE")
	if repoSource == "" {
		repoSource = "salesforce"
	}

	switch repoSource {
	case "mock":
		slog.InfoContext(ctx, "initialising mock B2B org reader")
		return mock.NewMockB2BOrgReader()

	case "salesforce":
		slog.InfoContext(ctx, "initialising Salesforce B2B org reader with sObject cache")
		sObjectClientInit(ctx) // also calls sfInit internally
		return salesforce.NewB2BOrgReader(sObjectClient, sfClient)

	default:
		log.Fatalf("unsupported REPOSITORY_SOURCE value: %q", repoSource)
		return nil
	}
}

// B2BOrgWriterImpl initialises and returns the port.B2BOrgWriter implementation
// selected by the REPOSITORY_SOURCE environment variable.
func B2BOrgWriterImpl(ctx context.Context) port.B2BOrgWriter {
	repoSource := os.Getenv("REPOSITORY_SOURCE")
	if repoSource == "" {
		repoSource = "salesforce"
	}

	switch repoSource {
	case "mock":
		slog.InfoContext(ctx, "initialising mock B2B org writer")
		return mock.NewMockB2BOrgWriter()

	case "salesforce":
		slog.InfoContext(ctx, "initialising Salesforce B2B org writer with sObject cache")
		sObjectClientInit(ctx)
		return salesforce.NewB2BOrgWriter(sObjectClient)

	default:
		log.Fatalf("unsupported REPOSITORY_SOURCE value: %q", repoSource)
		return nil
	}
}

// ProjectMembershipReaderImpl initialises and returns the port.ProjectMembershipReader
// implementation selected by the REPOSITORY_SOURCE environment variable:
//
//   - "salesforce" (default) — Salesforce sObject REST API reader.
//   - "mock"                 — Stub that always returns not-found; for local development without SF credentials.
func ProjectMembershipReaderImpl(ctx context.Context) port.ProjectMembershipReader {
	repoSource := os.Getenv("REPOSITORY_SOURCE")
	if repoSource == "" {
		repoSource = "salesforce"
	}

	switch repoSource {
	case "mock":
		slog.InfoContext(ctx, "initialising mock project membership reader")
		return mock.NewMockProjectMembershipReader()

	case "salesforce":
		slog.InfoContext(ctx, "initialising Salesforce project membership reader with sObject cache")
		sObjectClientInit(ctx)
		return salesforce.NewProjectMembershipReader(sObjectClient)

	default:
		log.Fatalf("unsupported REPOSITORY_SOURCE value: %q", repoSource)
		return nil
	}
}

// messagingSource returns the MESSAGING_SOURCE env var, defaulting to "nats".
// Used by all messaging-backed provider functions to select their implementation.
func messagingSource() string {
	if s := os.Getenv("MESSAGING_SOURCE"); s != "" {
		return s
	}
	return "nats"
}

// MemberPublisherImpl initialises and returns the port.MemberPublisher
// implementation selected by the MESSAGING_SOURCE environment variable:
//
//   - "nats" (default) — NATS JetStream publisher.
//   - "mock"           — No-op publisher that logs published messages.
//
// When MESSAGING_SOURCE=mock and GLOBAL_ORG_ADMIN_TEAM_UID is empty, the
// service still starts successfully — useful for local development.
func MemberPublisherImpl(ctx context.Context) port.MemberPublisher {
	switch messagingSource() {
	case "mock":
		slog.InfoContext(ctx, "initialising mock member publisher")
		return mock.NewMockMemberPublisher()

	case "nats":
		slog.InfoContext(ctx, "initialising NATS member publisher")
		natsInit(ctx)
		return nats.NewMessagePublisher(natsClient)

	default:
		log.Fatalf("unsupported MESSAGING_SOURCE value: %q", messagingSource())
		return nil
	}
}

// UserReaderImpl returns the port.UserReader implementation selected by the
// REPOSITORY_SOURCE environment variable:
//
//   - "salesforce" (default) — NATS RPC to auth-service.
//   - "mock"                 — No-op that always returns empty sub.
func UserReaderImpl(ctx context.Context) port.UserReader {
	repoSource := os.Getenv("REPOSITORY_SOURCE")
	if repoSource == "" {
		repoSource = "salesforce"
	}

	switch repoSource {
	case "mock":
		slog.InfoContext(ctx, "initialising mock user reader")
		return mock.NewMockUserReader()

	case "salesforce":
		slog.InfoContext(ctx, "initialising NATS user reader (auth-service)")
		natsInit(ctx)
		return nats.NewUserReader(natsClient)

	default:
		log.Fatalf("unsupported REPOSITORY_SOURCE value: %q", repoSource)
		return nil
	}
}

// GlobalOrgAdminTeamUID reads the GLOBAL_ORG_ADMIN_TEAM_UID environment variable.
// Returns empty string when not set (allowed in mock/messaging=mock mode; the
// FGA message simply omits the global_org_admin reference).
func GlobalOrgAdminTeamUID() string {
	return os.Getenv("GLOBAL_ORG_ADMIN_TEAM_UID")
}

// BackfillIteratorImpl returns the BackfillIterator implementation selected by
// the REPOSITORY_SOURCE environment variable:
//
//   - "salesforce" (default) — Salesforce SOQL paged iterators.
//   - "mock"                 — In-memory mock with no pre-loaded pages.
func BackfillIteratorImpl(ctx context.Context) usecaseSvc.BackfillIterator {
	repoSource := os.Getenv("REPOSITORY_SOURCE")
	if repoSource == "" {
		repoSource = "salesforce"
	}

	switch repoSource {
	case "mock":
		slog.InfoContext(ctx, "initialising mock backfill iterator")
		return &mock.MockBackfillIterator{}

	case "salesforce":
		slog.InfoContext(ctx, "initialising Salesforce backfill iterator")
		sfInit(ctx)
		return salesforce.NewBackfillIterator(
			salesforce.NewAccountRepo(sfClient),
			salesforce.NewMembershipRepo(sfClient),
			salesforce.NewKeyContactRepo(sfClient),
		)

	default:
		log.Fatalf("unsupported REPOSITORY_SOURCE value: %q", repoSource)
		return nil
	}
}

// mockSettingsInstance returns the shared MockB2BOrgSettings singleton.
// Reader and writer must share the same instance so writes are visible to reads.
func mockSettingsInstance(ctx context.Context) *mock.MockB2BOrgSettings {
	mockSettingsOnce.Do(func() {
		slog.InfoContext(ctx, "initialising mock B2B org settings store")
		mockSettings = mock.NewMockB2BOrgSettings()
	})
	return mockSettings
}

// B2BOrgSettingsReaderImpl returns the port.B2BOrgSettingsReader implementation
// selected by the REPOSITORY_SOURCE environment variable:
//
//   - "salesforce" (default) — NATS KV "org-settings" bucket (authoritative, no MaxAge TTL).
//   - "mock"                 — Shared in-memory mock; lets the service start without NATS.
func B2BOrgSettingsReaderImpl(ctx context.Context) port.B2BOrgSettingsReader {
	if os.Getenv("REPOSITORY_SOURCE") == "mock" {
		return mockSettingsInstance(ctx)
	}
	natsInit(ctx)
	return nats.NewStorage(natsClient)
}

// B2BOrgSettingsWriterImpl returns the port.B2BOrgSettingsWriter implementation
// selected by the REPOSITORY_SOURCE environment variable:
//
//   - "salesforce" (default) — NATS KV "org-settings" bucket.
//   - "mock"                 — Shared in-memory mock; same instance as B2BOrgSettingsReaderImpl.
func B2BOrgSettingsWriterImpl(ctx context.Context) port.B2BOrgSettingsWriter {
	if os.Getenv("REPOSITORY_SOURCE") == "mock" {
		return mockSettingsInstance(ctx)
	}
	natsInit(ctx)
	return nats.NewStorage(natsClient)
}

// BackfillRunnerImpl constructs a BackfillRunner wired with all production
// (or mock) dependencies based on REPOSITORY_SOURCE / MESSAGING_SOURCE.
//
// In mock mode natsClient is nil — the runner skips the distributed full-run
// lock (safe for local development; no concurrent runners exist).
func BackfillRunnerImpl(ctx context.Context) *usecaseSvc.Runner {
	repoSource := os.Getenv("REPOSITORY_SOURCE")
	if repoSource == "" {
		repoSource = "salesforce"
	}

	var kcReader usecaseSvc.KeyContactSObjectReader
	var nc *nats.NATSClient

	switch repoSource {
	case "mock":
		slog.InfoContext(ctx, "initialising mock backfill runner")
		kcReader = mock.NewMockKeyContactSObjectReader()

	case "salesforce":
		slog.InfoContext(ctx, "initialising Salesforce backfill runner")
		natsInit(ctx)
		nc = natsClient
		sObjectClientInit(ctx)
		kcReader = salesforce.NewKeyContactReader(sObjectClient)

	default:
		log.Fatalf("unsupported REPOSITORY_SOURCE value: %q", repoSource)
	}

	return usecaseSvc.NewRunner(
		BackfillIteratorImpl(ctx),
		B2BOrgReaderImpl(ctx),
		ProjectMembershipReaderImpl(ctx),
		kcReader,
		B2BOrgSettingsReaderImpl(ctx),
		MemberPublisherImpl(ctx),
		nc,
		GlobalOrgAdminTeamUID(),
		ProjectResolverImpl(ctx),
		// Avatar enrichment collaborators (b2b_org_settings + EnrichAvatars path / avatar-backfill Job).
		usecaseSvc.WithSettingsWriter(B2BOrgSettingsWriterImpl(ctx)),
		usecaseSvc.WithUserReader(UserReaderImpl(ctx)),
	)
}

// JWTAuthImpl constructs the domain.Authenticator from environment variables.
// Calls log.Fatalf on configuration or key-fetch errors — same fail-fast
// pattern as the other provider functions.
func JWTAuthImpl(ctx context.Context) domain.Authenticator {
	cfg := auth.JWTAuthConfig{
		JWKSURL:            os.Getenv("JWKS_URL"),
		Audience:           os.Getenv("AUDIENCE"),
		MockLocalPrincipal: os.Getenv("JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL"),
	}
	a, err := auth.NewJWTAuth(cfg)
	if err != nil {
		log.Fatalf("failed to set up JWT authentication: %v", err)
	}
	return a
}

// MemberReaderUseCase constructs the MemberReaderOrchestrator use-case wired
// with the production (or mock) MemberReader adapter.
func MemberReaderUseCase(ctx context.Context) usecaseSvc.MemberReader {
	return usecaseSvc.NewMemberReaderOrchestrator(
		usecaseSvc.WithMemberReader(MemberReaderImpl(ctx)),
	)
}

// B2BOrgWriterUseCase constructs the B2BOrgWriter use-case orchestrator wired
// with all production (or mock) dependencies.
func B2BOrgWriterUseCase(ctx context.Context) usecaseSvc.B2BOrgWriter {
	return usecaseSvc.NewB2BOrgWriter(
		usecaseSvc.WithB2BOrgReader(B2BOrgReaderImpl(ctx)),
		usecaseSvc.WithB2BOrgWriter(B2BOrgWriterImpl(ctx)),
		usecaseSvc.WithB2BOrgPublisher(MemberPublisherImpl(ctx)),
		usecaseSvc.WithGlobalOrgAdminTeamUID(GlobalOrgAdminTeamUID()),
	)
}

// KeyContactWriterUseCase constructs the KeyContactWriter use-case orchestrator.
func KeyContactWriterUseCase(ctx context.Context) usecaseSvc.KeyContactWriter {
	return usecaseSvc.NewKeyContactWriter(
		usecaseSvc.WithKCStorage(MemberReaderImpl(ctx)),
		usecaseSvc.WithKCWriter(KeyContactWriterImpl(ctx)),
		usecaseSvc.WithKCProjectMembershipReader(ProjectMembershipReaderImpl(ctx)),
		usecaseSvc.WithKCPublisher(MemberPublisherImpl(ctx)),
		usecaseSvc.WithKCUserReader(UserReaderImpl(ctx)),
		usecaseSvc.WithKCOrgSettings(OrgSettingsWriterUseCase(ctx)),
	)
}

// InviteSenderImpl returns the port.InviteSender implementation selected by the
// MESSAGING_SOURCE environment variable:
//
//   - "nats" (default) — NATS request/reply to the invite service.
//   - "mock"           — No-op that always succeeds; for local development.
func InviteSenderImpl(ctx context.Context) port.InviteSender {
	switch messagingSource() {
	case "mock":
		slog.InfoContext(ctx, "initialising mock invite sender")
		return mock.NewNoopInviteSender()

	case "nats":
		slog.InfoContext(ctx, "initialising NATS invite sender")
		natsInit(ctx)
		return nats.NewInviteSender(natsClient)

	default:
		log.Fatalf("unsupported MESSAGING_SOURCE value: %q", messagingSource())
		return nil
	}
}

// OrgRoleNotifierImpl returns the port.OrgRoleNotifier implementation selected
// by the MESSAGING_SOURCE environment variable:
//
//   - "nats" (default) — NATS request/reply to the email service.
//   - "mock"           — No-op that always succeeds; for local development.
func OrgRoleNotifierImpl(ctx context.Context) port.OrgRoleNotifier {
	switch messagingSource() {
	case "mock":
		slog.InfoContext(ctx, "initialising mock org role notifier")
		return mock.NewNoopOrgRoleNotifier()

	case "nats":
		slog.InfoContext(ctx, "initialising NATS org role notifier")
		natsInit(ctx)
		var orgDashboardURL string
		if base := strings.TrimRight(os.Getenv("LFX_SELF_SERVE_BASE_URL"), "/"); base != "" {
			orgDashboardURL = base + "/org"
		}
		return nats.NewOrgRoleNotifier(natsClient, orgDashboardURL)

	default:
		log.Fatalf("unsupported MESSAGING_SOURCE value: %q", messagingSource())
		return nil
	}
}

// mockWorkspacesInstance returns (or lazily creates) the shared MockOrgWorkspaces
// instance used by both the reader and writer in mock mode.
func mockWorkspacesInstance(ctx context.Context) *mock.MockOrgWorkspaces {
	mockWorkspacesOnce.Do(func() {
		slog.InfoContext(ctx, "initialising mock org workspaces store")
		mockWorkspaces = mock.NewMockOrgWorkspaces()
	})
	return mockWorkspaces
}

// OrgWorkspacesReaderImpl returns the port.OrgWorkspacesReader implementation.
func OrgWorkspacesReaderImpl(ctx context.Context) port.OrgWorkspacesReader {
	if os.Getenv("REPOSITORY_SOURCE") == "mock" {
		return mockWorkspacesInstance(ctx)
	}
	natsInit(ctx)
	return nats.NewStorage(natsClient)
}

// OrgWorkspacesWriterImpl returns the port.OrgWorkspacesWriter implementation.
func OrgWorkspacesWriterImpl(ctx context.Context) port.OrgWorkspacesWriter {
	if os.Getenv("REPOSITORY_SOURCE") == "mock" {
		return mockWorkspacesInstance(ctx)
	}
	natsInit(ctx)
	return nats.NewStorage(natsClient)
}

var mockWorkspaceProjectsOnce sync.Once
var mockWorkspaceProjectsStore *mock.MockWorkspaceProjects

func mockWorkspaceProjectsInstance(_ context.Context) *mock.MockWorkspaceProjects {
	mockWorkspaceProjectsOnce.Do(func() {
		mockWorkspaceProjectsStore = mock.NewMockWorkspaceProjects()
	})
	return mockWorkspaceProjectsStore
}

// WorkspaceProjectsReaderImpl returns the port.WorkspaceProjectsReader implementation.
func WorkspaceProjectsReaderImpl(ctx context.Context) port.WorkspaceProjectsReader {
	if os.Getenv("REPOSITORY_SOURCE") == "mock" {
		return mockWorkspaceProjectsInstance(ctx)
	}
	natsInit(ctx)
	return nats.NewStorage(natsClient)
}

// WorkspaceProjectsWriterImpl returns the port.WorkspaceProjectsWriter implementation.
func WorkspaceProjectsWriterImpl(ctx context.Context) port.WorkspaceProjectsWriter {
	if os.Getenv("REPOSITORY_SOURCE") == "mock" {
		return mockWorkspaceProjectsInstance(ctx)
	}
	natsInit(ctx)
	return nats.NewStorage(natsClient)
}

// WorkspaceWriterUseCase constructs the WorkspaceWriter use-case orchestrator.
func WorkspaceWriterUseCase(ctx context.Context) usecaseSvc.WorkspaceWriter {
	return usecaseSvc.NewWorkspaceWriter(
		usecaseSvc.WithWorkspacesReader(OrgWorkspacesReaderImpl(ctx)),
		usecaseSvc.WithWorkspacesWriter(OrgWorkspacesWriterImpl(ctx)),
		usecaseSvc.WithWorkspaceProjectsReader(WorkspaceProjectsReaderImpl(ctx)),
		usecaseSvc.WithWorkspaceProjectsWriter(WorkspaceProjectsWriterImpl(ctx)),
		usecaseSvc.WithWorkspacesB2BOrgReader(B2BOrgReaderImpl(ctx)),
		usecaseSvc.WithWorkspacesPublisher(MemberPublisherImpl(ctx)),
	)
}

// OrgSettingsWriterUseCase constructs the OrgSettingsWriter use-case orchestrator.
func OrgSettingsWriterUseCase(ctx context.Context) usecaseSvc.OrgSettingsWriter {
	return usecaseSvc.NewOrgSettingsWriter(
		usecaseSvc.WithOrgSettingsReader(B2BOrgSettingsReaderImpl(ctx)),
		usecaseSvc.WithOrgSettingsWriter(B2BOrgSettingsWriterImpl(ctx)),
		usecaseSvc.WithOrgSettingsB2BOrgReader(B2BOrgReaderImpl(ctx)),
		usecaseSvc.WithOrgSettingsPublisher(MemberPublisherImpl(ctx)),
		usecaseSvc.WithOrgSettingsUserReader(UserReaderImpl(ctx)),
		usecaseSvc.WithOrgSettingsInviteSender(InviteSenderImpl(ctx)),
		usecaseSvc.WithOrgSettingsRoleNotifier(OrgRoleNotifierImpl(ctx)),
		usecaseSvc.WithOrgSettingsSelfServeBaseURL(os.Getenv("LFX_SELF_SERVE_BASE_URL")),
	)
}

// InviteAcceptedServiceImpl constructs the InviteAcceptedService wired with all
// production (or mock) dependencies. The returned service is used by runAPI to
// handle lfx.invite-service.invite_accepted NATS events.
func InviteAcceptedServiceImpl(ctx context.Context) *usecaseSvc.InviteAcceptedService {
	return usecaseSvc.NewInviteAcceptedService(
		usecaseSvc.WithInviteAcceptedSettingsReader(B2BOrgSettingsReaderImpl(ctx)),
		usecaseSvc.WithInviteAcceptedOrgSettingsWriter(OrgSettingsWriterUseCase(ctx)),
		usecaseSvc.WithInviteAcceptedKeyContactReader(MemberReaderImpl(ctx)),
		usecaseSvc.WithInviteAcceptedPublisher(MemberPublisherImpl(ctx)),
	)
}

// QueueSubscriptions registers all runAPI NATS subscriptions. It initialises
// NATS, registers inbound RPC handlers (project-id-map when not in mock mode;
// b2b_org_lookup whenever a B2BOrgReader is wired, including mock mode), and
// always registers the invite_accepted handler. Drain callbacks are collected in
// apiSubs; call DrainAPISubscriptions on shutdown.
func QueueSubscriptions(ctx context.Context) error {
	natsInit(ctx)

	// project-id-map: only registered when a real resolver is wired (nil in mock mode).
	if resolver := ProjectResolverImpl(ctx); resolver != nil {
		sub, err := nats.SubscribeProjectIDMap(natsClient.Conn(), resolver)
		if err != nil {
			return fmt.Errorf("subscribe project-id-map: %w", err)
		}
		apiSubs = append(apiSubs, sub.Drain)
	}

	// b2b_org_lookup: resolve organization ids to indexed b2b_orgs (LFXV2-2400).
	if reader := B2BOrgReaderImpl(ctx); reader != nil {
		sub, err := nats.SubscribeB2BOrgLookup(natsClient.Conn(), reader)
		if err != nil {
			return fmt.Errorf("subscribe b2b_org_lookup: %w", err)
		}
		apiSubs = append(apiSubs, sub.Drain)
	}

	// invite_accepted: always registered; mock mode wires a no-op invite sender.
	invSub, err := nats.SubscribeInviteAccepted(natsClient.Conn(), InviteAcceptedServiceImpl(ctx).Handle)
	if err != nil {
		return fmt.Errorf("subscribe invite_accepted: %w", err)
	}
	apiSubs = append(apiSubs, invSub.Drain)

	return nil
}

// DrainAPISubscriptions drains all NATS subscriptions registered by QueueSubscriptions.
// Errors are logged and not returned — shutdown should proceed regardless.
func DrainAPISubscriptions(ctx context.Context) {
	for _, drain := range apiSubs {
		if err := drain(); err != nil {
			slog.WarnContext(ctx, "error draining NATS subscription", "error", err)
		}
	}
}

// B2BOrgResolverImpl returns a B2BOrgResolver that translates Salesforce Account
// SFIDs to v2 b2b_org UUIDs via a deterministic base-62 transform (no I/O).
// Unlike other providers, there is no mock/salesforce distinction — the resolver
// is pure CPU and works identically in every mode.
func B2BOrgResolverImpl(_ context.Context) port.B2BOrgResolver {
	return infrab2borg.NewResolver()
}

// CDCConsumerImpl constructs a CDCConsumer wired with all production
// dependencies for consumer mode. It also initialises the pubsub-state KV
// bucket (replay cursor storage) in the shared NATSClient.
//
// Required env vars (consumer mode only):
//
//	SF_PUBSUB_ENDPOINT — Salesforce Pub/Sub gRPC endpoint (e.g. "api.pubsub.salesforce.com:7443")
//	SF_ORG_ID          — Salesforce org ID / tenantid injected as gRPC metadata header
func CDCConsumerImpl(ctx context.Context) (*usecaseSvc.CDCConsumer, *pubsub.ReplayStore, *pubsub.Client) {
	endpoint := os.Getenv("SF_PUBSUB_ENDPOINT")
	if endpoint == "" {
		log.Fatalf("SF_PUBSUB_ENDPOINT is required in consumer mode")
	}

	orgID := os.Getenv("SF_ORG_ID")
	if orgID == "" {
		log.Fatalf("SF_ORG_ID is required in consumer mode (Salesforce org/tenant ID for gRPC metadata)")
	}

	// Init all shared singletons.
	natsInit(ctx)
	sfInit(ctx)
	sObjectClientInit(ctx)

	// Init pubsub-state KV bucket (no MaxAge TTL — replay cursors must survive indefinitely).
	if err := natsClient.KeyValueStore(ctx, constants.KVBucketNamePubSubState); err != nil {
		log.Fatalf("failed to initialise pubsub-state KV bucket: %v", err)
	}
	slog.InfoContext(ctx, "pubsub-state KV bucket initialised", "bucket", constants.KVBucketNamePubSubState)

	kv := natsClient.PubSubStateKV()
	if kv == nil {
		log.Fatalf("pubsub-state KV not available after initialisation")
	}
	replayStore := pubsub.NewReplayStore(kv)

	// tokenFn is called each time a new gRPC stream is opened so the access
	// token is always fresh (Salesforce sessions expire after a few hours).
	tokenFn := func() (accessToken, instanceURL, tenantID string) {
		return sfClient.GetAccessToken(), sfClient.GetInstanceUrl(), orgID
	}

	pubsubClient, err := pubsub.NewClient(endpoint, tokenFn)
	if err != nil {
		log.Fatalf("failed to create Pub/Sub gRPC client: %v", err)
	}
	slog.InfoContext(ctx, "Salesforce Pub/Sub gRPC client initialised", "endpoint", endpoint)

	// SOQL repos for the batched CDC re-fetch path (one query per event instead
	// of 4–6 sObject GETs per record). These repos are uncached — they sit
	// below the MemberReader stale-while-revalidate layer and call client.Query
	// directly — so the published record always reflects the CDC change.
	cdcMembershipRepo := salesforce.NewMembershipRepo(sfClient)
	cdcKeyContactRepo := salesforce.NewKeyContactRepo(sfClient)
	cdcAccountRepo := salesforce.NewAccountRepo(sfClient)

	consumer := usecaseSvc.NewCDCConsumer(
		usecaseSvc.WithCDCSubscriber(pubsubClient),
		usecaseSvc.WithCDCMemberReader(MemberReaderImpl(ctx)),
		// ProjectMembershipReaderImpl uses the sObject REST path (Asset + Account +
		// Product2 + Project__c) so that, after cache invalidation, the re-fetch
		// bypasses the membership-cache TTL and reads the changed record directly
		// from Salesforce. Using MemberReaderImpl here would read through the
		// stale membership-cache and publish pre-CDC data.
		usecaseSvc.WithCDCProjectMembershipReader(ProjectMembershipReaderImpl(ctx)),
		usecaseSvc.WithCDCB2BOrgReader(B2BOrgReaderImpl(ctx)),
		// Batch SOQL readers — replace the per-record sObject fan-out with a
		// single IN-clause query per CDC event (~4–6× fewer REST calls).
		usecaseSvc.WithCDCMembershipBatchReader(cdcMembershipRepo),
		usecaseSvc.WithCDCKeyContactBatchReader(cdcKeyContactRepo),
		usecaseSvc.WithCDCAccountBatchReader(cdcAccountRepo),
		// Quota guard — skips upsert fetches when the shared daily REST API
		// quota approaches exhaustion; /admin/reindex repairs skipped records.
		usecaseSvc.WithCDCQuotaGauge(salesforce.NewAPIUsageGauge()),
		usecaseSvc.WithCDCCacheInvalidator(sObjectClient),
		usecaseSvc.WithCDCPublisher(MemberPublisherImpl(ctx)),
		usecaseSvc.WithCDCGlobalOrgAdminTeamUID(GlobalOrgAdminTeamUID()),
		usecaseSvc.WithCDCUserReader(UserReaderImpl(ctx)),
		usecaseSvc.WithCDCOrgSettings(OrgSettingsWriterUseCase(ctx)),
	)

	return consumer, replayStore, pubsubClient
}

// CDCChannelFromEnv returns the Salesforce CDC channel to subscribe to.
// Defaults to "/data/ChangeEvents" (all CDC objects) when SF_CDC_CHANNEL is
// not set. Override in the consumer Deployment to subscribe to a specific
// channel (e.g. "/data/AccountChangeEvent").
func CDCChannelFromEnv() string {
	if ch := os.Getenv("SF_CDC_CHANNEL"); ch != "" {
		return ch
	}
	return "/data/ChangeEvents"
}
