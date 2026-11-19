package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"seedmirage/internal/announce"
	"seedmirage/internal/clock"
)

// maxParallelStops borne le nombre de « stopped » envoyés simultanément.
const maxParallelStops = 8

// ErrStopTimeout : le délai global d'arrêt (10 s, horloge injectée) est écoulé.
var ErrStopTimeout = errors.New("engine: délai d'arrêt dépassé")

type stopper interface {
	Stop(ctx context.Context) error
}

var _ stopper = (*announce.Announcer)(nil)

// stopAll arrête les éléments en parallèle, au plus limit à la fois, et rend
// la main quand tous ont fini ou que ctx est terminé (Stop avec un contexte
// terminé ferme l'announcer sans attendre). errs est aligné sur items.
func stopAll(ctx context.Context, items []stopper, limit int) []error {
	errs := make([]error, len(items))
	sem := make(chan struct{}, max(limit, 1))
	var wg sync.WaitGroup
	for i, it := range items {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			errs[i] = it.Stop(ctx)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = it.Stop(ctx)
		}()
	}
	wg.Wait()
	return errs
}

// timeoutContext dérive de parent un contexte annulé (cause ErrStopTimeout)
// après d sur l'horloge injectée. release libère la goroutine auxiliaire et
// l'attend ; à appeler toujours.
func timeoutContext(parent context.Context, clk clock.Clock, d time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	tm := clk.NewTimer(d)
	stop := make(chan struct{})
	fin := make(chan struct{})
	go func() {
		defer close(fin)
		select {
		case <-tm.C():
			cancel(ErrStopTimeout)
		case <-ctx.Done():
		case <-stop:
		}
	}()
	return ctx, func() {
		close(stop)
		<-fin
		tm.Stop()
		cancel(nil)
	}
}
