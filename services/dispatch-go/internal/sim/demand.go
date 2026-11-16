package sim

import (
	"math"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

// defaultCenter is the Bengaluru centre the hotspot offsets were measured from.
var defaultCenter = geo.Point{Lat: 12.9716, Lng: 77.5946}

// ConcertVenue is M. Chinnaswamy Stadium (shifted with CITY_CENTER_*).
var ConcertVenue = geo.Point{Lat: 12.9788, Lng: 77.5996}

type hotspot struct {
	name   string
	at     geo.Point
	weight float64
}

// Bengaluru demand hotspots, all within ~12 km of the centre.
var baseHotspots = []hotspot{
	{"MG Road", geo.Point{Lat: 12.9756, Lng: 77.6066}, 1.2},
	{"Koramangala", geo.Point{Lat: 12.9352, Lng: 77.6245}, 1.3},
	{"Indiranagar", geo.Point{Lat: 12.9784, Lng: 77.6408}, 1.2},
	{"Majestic", geo.Point{Lat: 12.9767, Lng: 77.5713}, 1.1},
	{"HSR Layout", geo.Point{Lat: 12.9116, Lng: 77.6474}, 1.0},
	{"Bellandur", geo.Point{Lat: 12.9304, Lng: 77.6784}, 1.0},
	{"Marathahalli", geo.Point{Lat: 12.9569, Lng: 77.7011}, 0.9},
	{"Jayanagar", geo.Point{Lat: 12.9250, Lng: 77.5938}, 0.9},
	{"BTM Layout", geo.Point{Lat: 12.9166, Lng: 77.6101}, 0.9},
	{"Malleshwaram", geo.Point{Lat: 13.0031, Lng: 77.5643}, 0.8},
	{"Hebbal", geo.Point{Lat: 13.0358, Lng: 77.5970}, 0.8},
	{"Yeshwanthpur", geo.Point{Lat: 13.0285, Lng: 77.5403}, 0.7},
}

// shift moves p by the offset between the configured and default centres.
func shift(p, center geo.Point) geo.Point {
	return geo.Point{Lat: p.Lat + center.Lat - defaultCenter.Lat, Lng: p.Lng + center.Lng - defaultCenter.Lng}
}

// cityRadiusKm bounds spawn points, wander targets and trips.
const cityRadiusKm = 12.0

// Hourly demand (IST), normalised so the daily mean is 1: morning and evening
// commute peaks, a flat afternoon and a quiet night.
var hourlyDemand = func() [24]float64 {
	raw := [24]float64{0.35, 0.25, 0.2, 0.2, 0.25, 0.4, 0.7, 1.1, 1.6, 1.7, 1.3, 1.1,
		1.05, 1.05, 1.0, 1.0, 1.2, 1.6, 1.8, 1.7, 1.4, 1.1, 0.8, 0.55}
	sum := 0.0
	for _, v := range raw {
		sum += v
	}
	for i := range raw {
		raw[i] *= 24 / sum
	}
	return raw
}()

var ist = time.FixedZone("IST", 5*3600+1800)

// TimeOfDayFactor is the demand multiplier at t (linear between hours, IST).
func TimeOfDayFactor(t time.Time) float64 {
	l := t.In(ist)
	h := l.Hour()
	f := (float64(l.Minute()) + float64(l.Second())/60) / 60
	return hourlyDemand[h]*(1-f) + hourlyDemand[(h+1)%24]*f
}

// Tier mix for the fleet and for demand: 15% AVs, the rest by weight.
var humanTiers = []struct {
	tier   fleet.Tier
	weight float64
}{
	{fleet.TierAuto, 20}, {fleet.TierMini, 25}, {fleet.TierSedan, 30}, {fleet.TierPremium, 13}, {fleet.TierXL, 12},
}

const avShare = 0.15

// spike is extra demand concentrated around a point.
type spike struct {
	name   string
	at     geo.Point
	perMin float64
	until  time.Time
}

func (s *Sim) poisson(lambda float64) int {
	if lambda <= 0 {
		return 0
	}
	if lambda > 30 { // normal approximation keeps this O(1)
		n := int(math.Round(lambda + math.Sqrt(lambda)*s.rng.NormFloat64()))
		return max(n, 0)
	}
	l, k, p := math.Exp(-lambda), 0, 1.0
	for {
		p *= s.rng.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

// jitter returns a point around c with a Gaussian spread of sigmaKm, kept inside the city.
func (s *Sim) jitter(c geo.Point, sigmaKm float64) geo.Point {
	for range 8 {
		p := offsetKm(c, s.rng.NormFloat64()*sigmaKm, s.rng.NormFloat64()*sigmaKm)
		if geo.HaversineKm(s.center, p) <= cityRadiusKm {
			return p
		}
	}
	return c
}

// offsetKm moves p by north/east kilometres.
func offsetKm(p geo.Point, northKm, eastKm float64) geo.Point {
	return geo.Point{
		Lat: p.Lat + northKm/110.574,
		Lng: p.Lng + eastKm/(111.320*math.Cos(p.Lat*math.Pi/180)),
	}
}

// pickHotspot picks a hotspot by weight, boosted by any spike nearby.
func (s *Sim) pickHotspot() hotspot {
	total := 0.0
	for _, h := range s.hotspots {
		total += h.weight
	}
	r := s.rng.Float64() * total
	for _, h := range s.hotspots {
		if r -= h.weight; r <= 0 {
			return h
		}
	}
	return s.hotspots[len(s.hotspots)-1]
}

func (s *Sim) pickTier() fleet.Tier {
	if s.rng.Float64() < avShare {
		return fleet.TierAV
	}
	return s.pickHumanTier()
}

func (s *Sim) pickHumanTier() fleet.Tier {
	total := 0.0
	for _, t := range humanTiers {
		total += t.weight
	}
	r := s.rng.Float64() * total
	for _, t := range humanTiers {
		if r -= t.weight; r <= 0 {
			return t.tier
		}
	}
	return fleet.TierSedan
}

// randomPickup draws a pickup: near an active spike (in proportion to its
// share of demand), near a hotspot, or anywhere in the city.
func (s *Sim) randomPickup(baseRate float64) geo.Point {
	total := baseRate
	for _, sp := range s.spikes {
		total += sp.perMin
	}
	r := s.rng.Float64() * total
	for _, sp := range s.spikes {
		if r < sp.perMin {
			return s.jitter(sp.at, 0.35)
		}
		r -= sp.perMin
	}
	if s.rng.Float64() < 0.8 {
		return s.jitter(s.pickHotspot().at, 1.0)
	}
	return s.jitter(s.center, 5)
}

// randomDropoff is 1.5-7 km from pickup, inside the city.
func (s *Sim) randomDropoff(pickup geo.Point) geo.Point {
	for range 8 {
		d := 1.5 + s.rng.Float64()*5.5
		a := s.rng.Float64() * 2 * math.Pi
		p := offsetKm(pickup, d*math.Cos(a), d*math.Sin(a))
		if geo.HaversineKm(s.center, p) <= cityRadiusKm {
			return p
		}
	}
	return s.jitter(s.pickHotspot().at, 0.8)
}
