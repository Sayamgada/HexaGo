package sim

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

var errPlannerBusy = errors.New("sim: route planner queue full")

// ticket is an asynchronous route request. The simulator polls it each tick
// and falls back to a straight line once its (simulation-time) deadline passes.
type ticket struct {
	from, to  geo.Point
	expires   time.Time // simulation time
	done      chan struct{}
	pts       []geo.Point
	err       error
	abandoned atomic.Bool
}

func (t *ticket) finish(pts []geo.Point, err error) {
	t.pts, t.err = pts, err
	close(t.done)
}

// result reports whether the ticket has completed.
func (t *ticket) result() ([]geo.Point, error, bool) {
	select {
	case <-t.done:
		return t.pts, t.err, true
	default:
		return nil, nil, false
	}
}

// planner runs route requests on a single worker so the public OSRM server
// sees at most its rate limit; ride legs jump ahead of idle wandering.
type planner struct {
	router  Router
	instant bool
	high    chan *ticket
	low     chan *ticket
	once    sync.Once
}

func newPlanner(r Router) *planner {
	_, instant := r.(StraightLine)
	if _, ok := r.(interface{ instant() }); ok { // synchronous test routers
		instant = true
	}
	return &planner{router: r, instant: instant, high: make(chan *ticket, 64), low: make(chan *ticket, 8)}
}

func (p *planner) start(ctx context.Context) {
	if p.instant {
		return
	}
	p.once.Do(func() { go p.work(ctx) })
}

func (p *planner) request(from, to geo.Point, urgent bool, expires time.Time) *ticket {
	t := &ticket{from: from, to: to, expires: expires, done: make(chan struct{})}
	if p.instant {
		pts, err := p.router.Route(context.Background(), from, to)
		t.finish(pts, err)
		return t
	}
	q := p.low
	if urgent {
		q = p.high
	}
	select {
	case q <- t:
	default:
		t.finish(nil, errPlannerBusy)
	}
	return t
}

func (p *planner) work(ctx context.Context) {
	for {
		var t *ticket
		select {
		case t = <-p.high:
		default:
			select {
			case t = <-p.high:
			case t = <-p.low:
			case <-ctx.Done():
				return
			}
		}
		if t.abandoned.Load() {
			t.finish(nil, context.Canceled)
			continue
		}
		pts, err := p.router.Route(ctx, t.from, t.to)
		t.finish(pts, err)
	}
}
