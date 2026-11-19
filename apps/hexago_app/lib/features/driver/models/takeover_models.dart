/// Take-over mode and Demand Forecast views:
/// GET /v1/forecast/heatmap, /v1/drivers/{id}/takeover, ride offers on the
/// `driver:<id>` stream topic, POST /v1/rides/{id}/offer/{accept|decline}.
library;

import 'dart:math' as math;

double _num(Object? v) => v is num ? v.toDouble() : double.tryParse('$v') ?? 0;
int _int(Object? v) => v is int ? v : (v is num ? v.round() : int.tryParse('$v') ?? 0);
String _str(Object? v) => v?.toString() ?? '';
Map<String, dynamic> _map(Object? v) => v is Map ? Map<String, dynamic>.from(v) : <String, dynamic>{};

/// Forecast horizons offered by the slider (minutes ahead).
const forecastHorizons = [0, 15, 30];

String horizonLabel(int h) => h == 0 ? 'Now' : '+$h';

/// One H3 res-8 cell of the demand heatmap.
class ForecastCell {
  const ForecastCell({
    required this.h3,
    required this.boundary,
    required this.centerLat,
    required this.centerLng,
    required this.demandScore,
    this.requestsPerHr = 0,
    this.earningsPerHr = 0,
    this.explanation = '',
    this.explanationSource = 'template',
    this.trend = 'steady',
  });

  final String h3;
  final List<(double, double)> boundary;
  final double centerLat;
  final double centerLng;

  /// 0..1 on an absolute scale (12 rides/h = 0.5).
  final double demandScore;
  final double requestsPerHr;
  final int earningsPerHr;
  final String explanation;
  final String explanationSource;
  final String trend;

  bool get fromLlm => explanationSource == 'llm';

  factory ForecastCell.fromJson(Map<String, dynamic> j) {
    final pts = <(double, double)>[
      for (final p in (j['boundary'] as List? ?? const []))
        if (p is List && p.length >= 2) (_num(p[0]), _num(p[1])),
    ];
    final c = _map(j['center']);
    var lat = _num(c['lat']), lng = _num(c['lng']);
    if (c.isEmpty && pts.isNotEmpty) {
      lat = pts.map((p) => p.$1).reduce((a, b) => a + b) / pts.length;
      lng = pts.map((p) => p.$2).reduce((a, b) => a + b) / pts.length;
    }
    return ForecastCell(
      h3: _str(j['h3']),
      boundary: pts,
      centerLat: lat,
      centerLng: lng,
      demandScore: _num(j['demand_score']).clamp(0, 1).toDouble(),
      requestsPerHr: _num(j['expected_requests_per_hr']),
      earningsPerHr: _int(j['expected_earnings_per_hr']),
      explanation: _str(j['explanation']),
      explanationSource: _str(j['explanation_source']).isEmpty ? 'template' : _str(j['explanation_source']),
      trend: _str(j['trend']).isEmpty ? 'steady' : _str(j['trend']),
    );
  }
}

class ForecastHeatmap {
  const ForecastHeatmap({required this.horizon, this.cells = const [], this.scenarios = const [], this.source = ''});

  final int horizon;
  final List<ForecastCell> cells;
  final List<String> scenarios;
  final String source;

  factory ForecastHeatmap.fromJson(Map<String, dynamic> j) => ForecastHeatmap(
        horizon: _int(j['horizon']),
        cells: [for (final c in (j['cells'] as List? ?? const [])) ForecastCell.fromJson(_map(c))],
        scenarios: [for (final s in (j['scenarios'] as List? ?? const [])) _str(s)],
        source: _str(j['source']),
      );
}

/// Great-circle distance in km.
double haversineKm(double lat1, double lng1, double lat2, double lng2) {
  const r = 6371.0;
  double rad(double d) => d * math.pi / 180;
  final dLat = rad(lat2 - lat1), dLng = rad(lng2 - lng1);
  final a = math.pow(math.sin(dLat / 2), 2) + math.cos(rad(lat1)) * math.cos(rad(lat2)) * math.pow(math.sin(dLng / 2), 2);
  return 2 * r * math.asin(math.sqrt(a));
}

/// A "Move to hotspot" suggestion.
class Hotspot {
  const Hotspot(this.cell, this.distanceKm);
  final ForecastCell cell;
  final double distanceKm;
}

/// The [n] cells where the driver earns most, ties broken by demand then distance.
List<Hotspot> topHotspots(List<ForecastCell> cells, double lat, double lng, {int n = 3}) {
  final all = [for (final c in cells) Hotspot(c, haversineKm(lat, lng, c.centerLat, c.centerLng))];
  all.sort((a, b) {
    final e = b.cell.earningsPerHr.compareTo(a.cell.earningsPerHr);
    if (e != 0) return e;
    final d = b.cell.demandScore.compareTo(a.cell.demandScore);
    return d != 0 ? d : a.distanceKm.compareTo(b.distanceKm);
  });
  return all.take(n).toList();
}

/// A ride offered to a taken-over driver, or its closing notice.
class RideOffer {
  const RideOffer({
    required this.type,
    required this.rideId,
    required this.driverId,
    this.riderId = '',
    this.pickupLat = 0,
    this.pickupLng = 0,
    this.dropLat = 0,
    this.dropLng = 0,
    this.fareInr = 0,
    this.tripKm = 0,
    this.pickupEtaSec = 0,
    this.timeoutS = 15,
    this.reason = '',
  });

  final String type; // ride_offer | offer_closed
  final String rideId;
  final String driverId;
  final String riderId;
  final double pickupLat;
  final double pickupLng;
  final double dropLat;
  final double dropLng;
  final double fareInr;
  final double tripKm;
  final int pickupEtaSec;
  final int timeoutS;
  final String reason; // offer_closed: accepted | declined | timeout | cancelled | released

  bool get isOffer => type == 'ride_offer';

  factory RideOffer.fromJson(Map<String, dynamic> j) {
    final p = _map(j['pickup']), d = _map(j['dropoff']);
    return RideOffer(
      type: _str(j['type']),
      rideId: _str(j['ride_id']),
      driverId: _str(j['driver_id']),
      riderId: _str(j['rider_id']),
      pickupLat: _num(p['lat']),
      pickupLng: _num(p['lng']),
      dropLat: _num(d['lat']),
      dropLng: _num(d['lng']),
      fareInr: _num(j['fare_inr']),
      tripKm: _num(j['trip_km']),
      pickupEtaSec: _int(j['pickup_eta_sec']),
      timeoutS: j['timeout_s'] == null ? 15 : _int(j['timeout_s']),
      reason: _str(j['reason']),
    );
  }
}

class TakeoverState {
  const TakeoverState({required this.driverId, this.humanControlled = false, this.ttlS = 0, this.offer});

  final String driverId;
  final bool humanControlled;
  final int ttlS;
  final RideOffer? offer;

  factory TakeoverState.fromJson(Map<String, dynamic> j) => TakeoverState(
        driverId: _str(j['driver_id']),
        humanControlled: j['human_controlled'] == true,
        ttlS: _int(j['ttl_s']),
        offer: j['offer'] is Map ? RideOffer.fromJson(_map(j['offer'])) : null,
      );
}

class OfferResult {
  const OfferResult({required this.rideId, required this.decision, this.status = '', this.rematchedTo = ''});

  final String rideId;
  final String decision;
  final String status;
  final String rematchedTo;

  factory OfferResult.fromJson(Map<String, dynamic> j) => OfferResult(
        rideId: _str(j['ride_id']),
        decision: _str(j['decision']),
        status: _str(j['status']),
        rematchedTo: _str(j['rematched_to']),
      );
}

/// A road route (OSRM, or a straight line).
class RoutePath {
  const RoutePath({this.points = const [], this.source = 'straight_line', this.distanceKm = 0});

  final List<(double, double)> points;
  final String source;
  final double distanceKm;

  factory RoutePath.fromJson(Map<String, dynamic> j) => RoutePath(
        points: [
          for (final p in (j['points'] as List? ?? const []))
            if (p is List && p.length >= 2) (_num(p[0]), _num(p[1])),
        ],
        source: _str(j['source']),
        distanceKm: _num(j['distance_km']),
      );
}

/// Why an offer closed, for the snack bar.
String offerClosedMessage(RideOffer o) => switch (o.reason) {
      'timeout' => 'Offer timed out: re-matched to another driver',
      'cancelled' => 'The rider cancelled the request',
      'released' => 'Offer returned to dispatch',
      'declined' => 'Offer declined: re-matched to another driver',
      _ => 'Offer closed',
    };
