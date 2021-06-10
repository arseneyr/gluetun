// Package dns defines interfaces to interact with DNS and DNS over TLS.
package dns

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/qdm12/dns/pkg/blacklist"
	"github.com/qdm12/dns/pkg/check"
	"github.com/qdm12/dns/pkg/nameserver"
	"github.com/qdm12/dns/pkg/unbound"
	"github.com/qdm12/gluetun/internal/configuration"
	"github.com/qdm12/gluetun/internal/constants"
	"github.com/qdm12/gluetun/internal/models"
	"github.com/qdm12/golibs/logging"
	"github.com/qdm12/golibs/os"
)

type Looper interface {
	Run(ctx context.Context, done chan<- struct{})
	RunRestartTicker(ctx context.Context, done chan<- struct{})
	GetStatus() (status models.LoopStatus)
	SetStatus(status models.LoopStatus) (outcome string, err error)
	GetSettings() (settings configuration.DNS)
	SetSettings(settings configuration.DNS) (outcome string)
}

type looper struct {
	state        state
	conf         unbound.Configurator
	blockBuilder blacklist.Builder
	client       *http.Client
	logger       logging.Logger
	loopLock     sync.Mutex
	start        chan struct{}
	running      chan models.LoopStatus
	stop         chan struct{}
	stopped      chan struct{}
	updateTicker chan struct{}
	backoffTime  time.Duration
	timeNow      func() time.Time
	timeSince    func(time.Time) time.Duration
	openFile     os.OpenFileFunc
}

const defaultBackoffTime = 10 * time.Second

func NewLooper(conf unbound.Configurator, settings configuration.DNS, client *http.Client,
	logger logging.Logger, openFile os.OpenFileFunc) Looper {
	return &looper{
		state: state{
			status:   constants.Stopped,
			settings: settings,
		},
		conf:         conf,
		blockBuilder: blacklist.NewBuilder(client),
		client:       client,
		logger:       logger,
		start:        make(chan struct{}),
		running:      make(chan models.LoopStatus),
		stop:         make(chan struct{}),
		stopped:      make(chan struct{}),
		updateTicker: make(chan struct{}),
		backoffTime:  defaultBackoffTime,
		timeNow:      time.Now,
		timeSince:    time.Since,
		openFile:     openFile,
	}
}

func (l *looper) logAndWait(ctx context.Context, err error) {
	if err != nil {
		l.logger.Warn(err)
	}
	l.logger.Info("attempting restart in %s", l.backoffTime)
	timer := time.NewTimer(l.backoffTime)
	l.backoffTime *= 2
	select {
	case <-timer.C:
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
	}
}

func (l *looper) Run(ctx context.Context, done chan<- struct{}) {
	defer close(done)

	const fallback = false
	l.useUnencryptedDNS(fallback) // TODO remove? Use default DNS by default for Docker resolution?
	// TODO this one is kept if DNS_KEEP_NAMESERVER=on and should be replaced

	select {
	case <-l.start:
	case <-ctx.Done():
		return
	}

	crashed := false
	l.backoffTime = defaultBackoffTime

	for ctx.Err() == nil {
		// Upper scope variables for Unbound only
		// Their values are to be used if DOT=off
		var waitError chan error
		var unboundCancel context.CancelFunc
		var closeStreams func()

		for l.GetSettings().Enabled {
			if ctx.Err() != nil {
				if !crashed {
					l.running <- constants.Stopped
				}
				return
			}
			var err error
			unboundCancel, waitError, closeStreams, err = l.setupUnbound(ctx, crashed)
			if err != nil {
				if !errors.Is(err, errUpdateFiles) {
					const fallback = true
					l.useUnencryptedDNS(fallback)
				}
				l.logAndWait(ctx, err)
				continue
			}
			break
		}
		if !l.GetSettings().Enabled {
			const fallback = false
			l.useUnencryptedDNS(fallback)
			waitError := make(chan error)
			unboundCancel = func() { waitError <- nil }
			closeStreams = func() {}
		}

		stayHere := true
		for stayHere {
			select {
			case <-ctx.Done():
				unboundCancel()
				<-waitError
				close(waitError)
				closeStreams()
				return
			case <-l.stop:
				l.logger.Info("stopping")
				const fallback = false
				l.useUnencryptedDNS(fallback)
				unboundCancel()
				<-waitError
				l.stopped <- struct{}{}
			case <-l.start:
				l.logger.Info("starting")
				stayHere = false
			case err := <-waitError: // unexpected error
				unboundCancel()
				if ctx.Err() != nil {
					close(waitError)
					closeStreams()
					return
				}
				l.state.setStatusWithLock(constants.Crashed)
				const fallback = true
				l.useUnencryptedDNS(fallback)
				l.logAndWait(ctx, err)
				stayHere = false
			}
		}
		close(waitError)
		closeStreams()
	}
}

var errUpdateFiles = errors.New("cannot update files")

// Returning cancel == nil signals we want to re-run setupUnbound
// Returning err == errUpdateFiles signals we should not fall back
// on the plaintext DNS as DOT is still up and running.
func (l *looper) setupUnbound(ctx context.Context, previousCrashed bool) (
	cancel context.CancelFunc, waitError chan error, closeStreams func(), err error) {
	err = l.updateFiles(ctx)
	if err != nil {
		l.state.setStatusWithLock(constants.Crashed)
		return nil, nil, nil, errUpdateFiles
	}

	settings := l.GetSettings()

	unboundCtx, cancel := context.WithCancel(context.Background())
	stdoutLines, stderrLines, waitError, err := l.conf.Start(unboundCtx, settings.Unbound.VerbosityDetailsLevel)
	if err != nil {
		cancel()
		if !previousCrashed {
			l.running <- constants.Crashed
		}
		return nil, nil, nil, err
	}

	collectLinesDone := make(chan struct{})
	go l.collectLines(stdoutLines, stderrLines, collectLinesDone)

	// use Unbound
	nameserver.UseDNSInternally(net.IP{127, 0, 0, 1})
	err = nameserver.UseDNSSystemWide(l.openFile,
		net.IP{127, 0, 0, 1}, settings.KeepNameserver)
	if err != nil {
		l.logger.Error(err)
	}

	if err := check.WaitForDNS(ctx, net.DefaultResolver); err != nil {
		if !previousCrashed {
			l.running <- constants.Crashed
		}
		cancel()
		<-waitError
		close(waitError)
		close(stdoutLines)
		close(stderrLines)
		<-collectLinesDone
		return nil, nil, nil, err
	}

	l.logger.Info("ready")
	if !previousCrashed {
		l.running <- constants.Running
	} else {
		l.backoffTime = defaultBackoffTime
		l.state.setStatusWithLock(constants.Running)
	}

	closeStreams = func() {
		close(stdoutLines)
		close(stderrLines)
		<-collectLinesDone
	}

	return cancel, waitError, closeStreams, nil
}

func (l *looper) useUnencryptedDNS(fallback bool) {
	settings := l.GetSettings()

	// Try with user provided plaintext ip address
	targetIP := settings.PlaintextAddress
	if targetIP != nil {
		if fallback {
			l.logger.Info("falling back on plaintext DNS at address %s", targetIP)
		} else {
			l.logger.Info("using plaintext DNS at address %s", targetIP)
		}
		nameserver.UseDNSInternally(targetIP)
		if err := nameserver.UseDNSSystemWide(l.openFile,
			targetIP, settings.KeepNameserver); err != nil {
			l.logger.Error(err)
		}
		return
	}

	provider := settings.Unbound.Providers[0]
	targetIP = provider.DoT().IPv4[0]
	if fallback {
		l.logger.Info("falling back on plaintext DNS at address " + targetIP.String())
	} else {
		l.logger.Info("using plaintext DNS at address " + targetIP.String())
	}
	nameserver.UseDNSInternally(targetIP)
	if err := nameserver.UseDNSSystemWide(l.openFile, targetIP, settings.KeepNameserver); err != nil {
		l.logger.Error(err)
	}
}

func (l *looper) RunRestartTicker(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	// Timer that acts as a ticker
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	timerIsStopped := true
	settings := l.GetSettings()
	if settings.UpdatePeriod > 0 {
		timer.Reset(settings.UpdatePeriod)
		timerIsStopped = false
	}
	lastTick := time.Unix(0, 0)
	for {
		select {
		case <-ctx.Done():
			if !timerIsStopped && !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
			lastTick = l.timeNow()

			status := l.GetStatus()
			if status == constants.Running {
				if err := l.updateFiles(ctx); err != nil {
					l.state.setStatusWithLock(constants.Crashed)
					l.logger.Error(err)
					l.logger.Warn("skipping Unbound restart due to failed files update")
					continue
				}
			}

			_, _ = l.SetStatus(constants.Stopped)
			_, _ = l.SetStatus(constants.Running)

			settings := l.GetSettings()
			timer.Reset(settings.UpdatePeriod)
		case <-l.updateTicker:
			if !timer.Stop() {
				<-timer.C
			}
			timerIsStopped = true
			settings := l.GetSettings()
			newUpdatePeriod := settings.UpdatePeriod
			if newUpdatePeriod == 0 {
				continue
			}
			var waited time.Duration
			if lastTick.UnixNano() != 0 {
				waited = l.timeSince(lastTick)
			}
			leftToWait := newUpdatePeriod - waited
			timer.Reset(leftToWait)
			timerIsStopped = false
		}
	}
}

func (l *looper) updateFiles(ctx context.Context) (err error) {
	l.logger.Info("downloading DNS over TLS cryptographic files")
	if err := l.conf.SetupFiles(ctx); err != nil {
		return err
	}
	settings := l.GetSettings()

	l.logger.Info("downloading hostnames and IP block lists")
	blockedHostnames, blockedIPs, blockedIPPrefixes, errs := l.blockBuilder.All(
		ctx, settings.BlacklistBuild)
	for _, err := range errs {
		l.logger.Warn(err)
	}

	// TODO change to BlockHostnames() when migrating to qdm12/dns v2
	settings.Unbound.Blacklist.FqdnHostnames = blockedHostnames
	settings.Unbound.Blacklist.IPs = blockedIPs
	settings.Unbound.Blacklist.IPPrefixes = blockedIPPrefixes

	return l.conf.MakeUnboundConf(settings.Unbound)
}
