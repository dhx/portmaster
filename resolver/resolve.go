package resolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/publicsuffix"

	"github.com/safing/portbase/database"
	"github.com/safing/portbase/log"
	"github.com/safing/portmaster/netenv"
)

// Errors.
var (
	// Basic Errors.

	// ErrNotFound is a basic error that will match all "not found" errors.
	ErrNotFound = errors.New("record could not be found")
	// ErrBlocked is basic error that will match all "blocked" errors.
	ErrBlocked = errors.New("query was blocked")
	// ErrLocalhost is returned to *.localhost queries.
	ErrLocalhost = errors.New("query for localhost")
	// ErrTimeout is returned when a query times out.
	ErrTimeout = errors.New("query timed out")
	// ErrOffline is returned when no network connection is detected.
	ErrOffline = errors.New("device is offine")
	// ErrFailure is returned when the type of failure is unclear.
	ErrFailure = errors.New("query failed")
	// ErrContinue is returned when the resolver has no answer, and the next resolver should be asked.
	ErrContinue = errors.New("resolver has no answer")
	// ErrShuttingDown is returned when the resolver is shutting down.
	ErrShuttingDown = errors.New("resolver is shutting down")

	// Detailed Errors.

	// ErrTestDomainsDisabled wraps ErrBlocked.
	ErrTestDomainsDisabled = fmt.Errorf("%w: test domains disabled", ErrBlocked)
	// ErrSpecialDomainsDisabled wraps ErrBlocked.
	ErrSpecialDomainsDisabled = fmt.Errorf("%w: special domains disabled", ErrBlocked)
	// ErrInvalid wraps ErrNotFound.
	ErrInvalid = fmt.Errorf("%w: invalid request", ErrNotFound)
	// ErrNoCompliance wraps ErrBlocked and is returned when no resolvers were able to comply with the current settings.
	ErrNoCompliance = fmt.Errorf("%w: no compliant resolvers for this query", ErrBlocked)
)

const (
	minTTL     = 60 // 1 Minute
	refreshTTL = minTTL / 2
	minMDnsTTL = 60           // 1 Minute
	maxTTL     = 24 * 60 * 60 // 24 hours
)

var (
	dupReqMap  = make(map[string]*dedupeStatus)
	dupReqLock sync.Mutex
)

type dedupeStatus struct {
	completed  chan struct{}
	waitUntil  time.Time
	superseded bool
}

// BlockedUpstreamError is returned when a DNS request
// has been blocked by the upstream server.
type BlockedUpstreamError struct {
	ResolverName string
}

func (blocked *BlockedUpstreamError) Error() string {
	return fmt.Sprintf("%s by upstream DNS resolver %s", ErrBlocked, blocked.ResolverName)
}

// Unwrap implements errors.Unwrapper.
func (blocked *BlockedUpstreamError) Unwrap() error {
	return ErrBlocked
}

// Query describes a dns query.
type Query struct {
	FQDN               string
	QType              dns.Type
	SecurityLevel      uint8
	NoCaching          bool
	IgnoreFailing      bool
	LocalResolversOnly bool

	// ICANNSpace signifies if the domain is within ICANN managed domain space.
	ICANNSpace bool
	// Domain root is the effective TLD +1.
	DomainRoot string

	// internal
	dotPrefixedFQDN string
}

// ID returns the ID of the query consisting of the domain and question type.
func (q *Query) ID() string {
	return q.FQDN + q.QType.String()
}

// InitPublicSuffixData initializes the public suffix data.
func (q *Query) InitPublicSuffixData() {
	// Get public suffix and derive if domain is in ICANN space.
	suffix, icann := publicsuffix.PublicSuffix(strings.TrimSuffix(q.FQDN, "."))
	if icann || strings.Contains(suffix, ".") {
		q.ICANNSpace = true
	}
	// Override some cases.
	switch suffix {
	case "example":
		q.ICANNSpace = true // Defined by ICANN.
	case "invalid":
		q.ICANNSpace = true // Defined by ICANN.
	case "local":
		q.ICANNSpace = true // Defined by ICANN.
	case "localhost":
		q.ICANNSpace = true // Defined by ICANN.
	case "onion":
		q.ICANNSpace = false // Defined by ICANN, but special.
	case "test":
		q.ICANNSpace = true // Defined by ICANN.
	}
	// Add suffix to adhere to FQDN format.
	suffix += "."

	switch {
	case len(q.FQDN) == len(suffix):
		// We are at or below the domain root, reset.
		q.DomainRoot = ""
	case len(q.FQDN) > len(suffix):
		domainRootStart := strings.LastIndex(q.FQDN[:len(q.FQDN)-len(suffix)-1], ".") + 1
		q.DomainRoot = q.FQDN[domainRootStart:]
	}
}

// check runs sanity checks and does some initialization. Returns whether the query passed the basic checks.
func (q *Query) check() (ok bool) {
	if q.FQDN == "" {
		return false
	}

	// init
	q.FQDN = dns.Fqdn(q.FQDN)
	if q.FQDN == "." {
		q.dotPrefixedFQDN = q.FQDN
	} else {
		q.dotPrefixedFQDN = "." + q.FQDN
	}

	return true
}

// Resolve resolves the given query for a domain and type and returns a RRCache object or nil, if the query failed.
func Resolve(ctx context.Context, q *Query) (rrCache *RRCache, err error) {
	// sanity check
	if q == nil || !q.check() {
		return nil, ErrInvalid
	}

	// log
	// try adding a context tracer
	ctx, tracer := log.AddTracer(ctx)
	defer tracer.Submit()
	log.Tracer(ctx).Tracef("resolver: resolving %s%s", q.FQDN, q.QType)

	// check query compliance
	if err = q.checkCompliance(); err != nil {
		return nil, err
	}

	// check the cache
	if !q.NoCaching {
		rrCache = checkCache(ctx, q)
		if rrCache != nil && !rrCache.Expired() {
			return rrCache, nil
		}

		// dedupe!
		markRequestFinished := deduplicateRequest(ctx, q)
		if markRequestFinished == nil {
			// we waited for another request, recheck the cache!
			rrCache = checkCache(ctx, q)
			if rrCache != nil && !rrCache.Expired() {
				return rrCache, nil
			}
			log.Tracer(ctx).Debugf("resolver: waited for another %s%s query, but cache missed!", q.FQDN, q.QType)
			// if cache is still empty or non-compliant, go ahead and just query
		} else {
			// we are the first!
			defer markRequestFinished()
		}
	}

	return resolveAndCache(ctx, q, rrCache)
}

func checkCache(ctx context.Context, q *Query) *RRCache {
	// Never ask cache for connectivity domains.
	if netenv.IsConnectivityDomain(q.FQDN) {
		return nil
	}

	// Get data from cache.
	rrCache, err := GetRRCache(q.FQDN, q.QType)
	// Return if entry is not in cache.
	if err != nil {
		if !errors.Is(err, database.ErrNotFound) {
			log.Tracer(ctx).Warningf("resolver: getting RRCache %s%s from database failed: %s", q.FQDN, q.QType.String(), err)
		}
		return nil
	}

	// Get the resolver that the rrCache was resolved with.
	resolver := getActiveResolverByIDWithLocking(rrCache.Resolver.ID())
	if resolver == nil {
		log.Tracer(ctx).Debugf("resolver: ignoring RRCache %s%s because source server %q has been removed", q.FQDN, q.QType.String(), rrCache.Resolver.ID())
		return nil
	}

	// Check compliance of the resolver, return if non-compliant.
	err = resolver.checkCompliance(ctx, q)
	if err != nil {
		log.Tracer(ctx).Debugf("resolver: cached entry for %s%s does not comply to query parameters: %s", q.FQDN, q.QType.String(), err)
		return nil
	}

	// Check if we want to reset the cache for this entry.
	if shouldResetCache(q) {
		err := ResetCachedRecord(q.FQDN, q.QType.String())
		switch {
		case err == nil:
			log.Tracer(ctx).Tracef("resolver: cache for %s%s was reset", q.FQDN, q.QType)
		case errors.Is(err, database.ErrNotFound):
			log.Tracer(ctx).Tracef("resolver: cache for %s%s was already reset (is empty)", q.FQDN, q.QType)
		default:
			log.Tracer(ctx).Warningf("resolver: failed to reset cache for %s%s: %s", q.FQDN, q.QType, err)
		}
		return nil
	}

	// Check if the cache has already expired.
	// We still return the cache, if it isn't NXDomain, as it will be used if the
	// new query fails.
	if rrCache.Expired() {
		if rrCache.RCode == dns.RcodeSuccess {
			return rrCache
		}
		return nil
	}

	// Check if the cache will expire soon and start an async request.
	if rrCache.ExpiresSoon() {
		// Set flag that we are refreshing this entry.
		rrCache.RequestingNew = true

		log.Tracer(ctx).Tracef(
			"resolver: cache for %s will expire in %s, refreshing async now",
			q.ID(),
			time.Until(time.Unix(rrCache.Expires, 0)).Round(time.Second),
		)

		// resolve async
		module.StartWorker("resolve async", func(asyncCtx context.Context) error {
			tracingCtx, tracer := log.AddTracer(asyncCtx)
			defer tracer.Submit()
			tracer.Tracef("resolver: resolving %s async", q.ID())
			_, err := resolveAndCache(tracingCtx, q, nil)
			if err != nil {
				tracer.Warningf("resolver: async query for %s failed: %s", q.ID(), err)
			} else {
				tracer.Infof("resolver: async query for %s succeeded", q.ID())
			}
			return nil
		})

		return rrCache
	}

	log.Tracer(ctx).Tracef(
		"resolver: using cached RR (expires in %s)",
		time.Until(time.Unix(rrCache.Expires, 0)).Round(time.Second),
	)
	return rrCache
}

func deduplicateRequest(ctx context.Context, q *Query) (finishRequest func()) {
	// create identifier key
	dupKey := q.ID()

	// restart here if waiting timed out
retry:

	dupReqLock.Lock()

	// get duplicate request waitgroup
	status, requestActive := dupReqMap[dupKey]

	// check if the request ist active
	if requestActive {
		// someone else is already on it!
		if time.Now().Before(status.waitUntil) {
			dupReqLock.Unlock()

			// log that we are waiting
			log.Tracer(ctx).Tracef("resolver: waiting for duplicate query for %s to complete", dupKey)
			// wait
			select {
			case <-status.completed:
				// done!
				return nil
			case <-time.After(maxRequestTimeout):
				// something went wrong with the query, retry
				goto retry
			case <-ctx.Done():
				return nil
			}
		} else {
			// but that someone is taking too long
			status.superseded = true
		}
	}

	// we are currently the only one doing a request for this

	// create new status
	status = &dedupeStatus{
		completed: make(chan struct{}),
		waitUntil: time.Now().Add(maxRequestTimeout),
	}
	// add to registry
	dupReqMap[dupKey] = status

	dupReqLock.Unlock()

	// return function to mark request as finished
	return func() {
		dupReqLock.Lock()
		defer dupReqLock.Unlock()
		// mark request as done
		close(status.completed)
		// delete from registry
		if !status.superseded {
			delete(dupReqMap, dupKey)
		}
	}
}

func resolveAndCache(ctx context.Context, q *Query, oldCache *RRCache) (rrCache *RRCache, err error) { //nolint:gocognit,gocyclo
	// get resolvers
	resolvers, primarySource, tryAll := GetResolversInScope(ctx, q)
	if len(resolvers) == 0 {
		return nil, ErrNoCompliance
	}

	// check if we are online
	if netenv.GetOnlineStatus() == netenv.StatusOffline && primarySource != ServerSourceEnv {
		if q.FQDN != netenv.DNSTestDomain && !netenv.IsConnectivityDomain(q.FQDN) {
			// we are offline and this is not an online check query
			return oldCache, ErrOffline
		}
		log.Tracer(ctx).Debugf("resolver: allowing online status test domain %s to resolve even though offline", q.FQDN)
	}

	// start resolving

	var i int
	// once with skipping recently failed resolvers, once without
resolveLoop:
	for i = 0; i < 2; i++ {
		for _, resolver := range resolvers {
			if module.IsStopping() {
				return nil, ErrShuttingDown
			}

			// check if resolver failed recently (on first run)
			if i == 0 && resolver.Conn.IsFailing() {
				log.Tracer(ctx).Tracef("resolver: skipping resolver %s, because it failed recently", resolver)
				continue
			}

			// resolve
			log.Tracer(ctx).Tracef("resolver: sending query for %s to %s", q.ID(), resolver.Info.ID())
			rrCache, err = resolver.Conn.Query(ctx, q)
			if err != nil {
				switch {
				case errors.Is(err, ErrNotFound):
					// NXDomain, or similar
					if tryAll {
						continue
					}
					return nil, err
				case errors.Is(err, ErrBlocked):
					// some resolvers might also block
					return nil, err
				case netenv.GetOnlineStatus() == netenv.StatusOffline &&
					q.FQDN != netenv.DNSTestDomain &&
					!netenv.IsConnectivityDomain(q.FQDN):
					// we are offline and this is not an online check query
					return oldCache, ErrOffline
				case errors.Is(err, ErrContinue):
					continue
				case errors.Is(err, ErrTimeout):
					resolver.Conn.ReportFailure()
					log.Tracer(ctx).Debugf("resolver: query to %s timed out", resolver.Info.ID())
					continue
				case errors.Is(err, context.Canceled):
					return nil, err
				case errors.Is(err, context.DeadlineExceeded):
					return nil, err
				case errors.Is(err, ErrShuttingDown):
					return nil, err
				default:
					resolver.Conn.ReportFailure()
					log.Tracer(ctx).Debugf("resolver: query to %s failed: %s", resolver.Info.ID(), err)
					continue
				}
			}
			if rrCache == nil {
				// Defensive: This should normally not happen.
				continue
			}
			// Check if request succeeded and whether we should try another resolver.
			if rrCache.RCode != dns.RcodeSuccess && tryAll {
				continue
			}

			// Report a successful connection.
			resolver.Conn.ResetFailure()
			// Reset failing resolvers notification, if querying in global scope.
			if primarySource == ServerSourceConfigured {
				resetFailingResolversNotification()
			}

			break resolveLoop
		}
	}

	// Post-process errors
	if err != nil {
		// tried all resolvers, possibly twice
		if i > 1 {
			err = fmt.Errorf("all %d query-compliant resolvers failed, last error: %w", len(resolvers), err)

			if primarySource == ServerSourceConfigured &&
				netenv.Online() && CompatSelfCheckIsFailing() {
				notifyAboutFailingResolvers(err)
			} else {
				resetFailingResolversNotification()
			}
		}
	} else if rrCache == nil /* defensive */ {
		err = ErrNotFound
	}

	// Check if we want to use an older cache instead.
	if oldCache != nil {
		oldCache.IsBackup = true

		switch {
		case err != nil:
			// There was an error during resolving, return the old cache entry instead.
			log.Tracer(ctx).Debugf("resolver: serving backup cache of %s because query failed: %s", q.ID(), err)
			return oldCache, nil
		case !rrCache.Cacheable():
			// The new result is NXDomain, return the old cache entry instead.
			log.Tracer(ctx).Debugf("resolver: serving backup cache of %s because fresh response is NXDomain", q.ID())
			return oldCache, nil
		}
	}

	// Return error, if there is one.
	if err != nil {
		return nil, err
	}

	// Adjust TTLs.
	rrCache.Clean(minTTL)

	// Save the new entry if cache is enabled and the record may be cached.
	if !q.NoCaching && rrCache.Cacheable() {
		err = rrCache.Save()
		if err != nil {
			log.Tracer(ctx).Warningf("resolver: failed to cache RR for %s: %s", q.ID(), err)
		}
	}

	return rrCache, nil
}

var (
	cacheResetLock    sync.Mutex
	cacheResetID      string
	cacheResetSeenCnt int
)

func shouldResetCache(q *Query) (reset bool) {
	cacheResetLock.Lock()
	defer cacheResetLock.Unlock()

	// reset to new domain
	qID := q.ID()
	if qID != cacheResetID {
		cacheResetID = qID
		cacheResetSeenCnt = 1
		return false
	}

	// increase and check if threshold is reached
	cacheResetSeenCnt++
	if cacheResetSeenCnt >= 3 { // 3 to trigger reset
		cacheResetSeenCnt = -7 // 10 for follow-up resets
		return true
	}

	return false
}

func init() {
	netenv.DNSTestQueryFunc = testConnectivity
}

// testConnectivity test if resolving a query succeeds and returns whether the
// query itself succeeded, separate from interpreting the result.
func testConnectivity(ctx context.Context, fdqn string) (ips []net.IP, ok bool, err error) {
	q := &Query{
		FQDN:      fdqn,
		QType:     dns.Type(dns.TypeA),
		NoCaching: true,
	}
	if !q.check() {
		return nil, false, ErrInvalid
	}

	rrCache, err := resolveAndCache(ctx, q, nil)
	switch {
	case err == nil:
		switch rrCache.RCode {
		case dns.RcodeNameError:
			return nil, true, ErrNotFound
		case dns.RcodeRefused:
			return nil, true, errors.New("refused")
		default:
			ips := rrCache.ExportAllARecords()
			if len(ips) > 0 {
				return ips, true, nil
			}
			return nil, true, ErrNotFound
		}
	case errors.Is(err, ErrNotFound):
		return nil, true, err
	case errors.Is(err, ErrBlocked):
		return nil, true, err
	case errors.Is(err, ErrNoCompliance):
		return nil, true, err
	default:
		return nil, false, err
	}
}
