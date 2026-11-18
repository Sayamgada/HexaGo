import 'dart:convert';

/// Wire models for the Ops Command Center.
///
/// Contract (dispatch-go gateway):
/// * `GET /v1/metrics/dispatch` -> [DispatchMetrics]
/// * WebSocket `/v1/stream` frames:
///   * `{"topic":"drivers","data":[DriverPosition...]}`
///   * `{"topic":"ops","data":{"type":"agent_event", ...AgentEvent}}`
///   * `{"topic":"ops","data":{"type":"metrics", ...DispatchMetrics}}`
///
/// Parsing is tolerant: numbers may arrive as int, double or numeric strings,
/// and unknown enum values fall back to an `unknown` case instead of throwing.

double _toDouble(Object? v, [double fallback = 0]) {
  if (v is num) return v.toDouble();
  if (v is String) return double.tryParse(v) ?? fallback;
  return fallback;
}

double? _toNullableDouble(Object? v) {
  if (v == null) return null;
  if (v is num) return v.toDouble();
  if (v is String) return double.tryParse(v);
  return null;
}

int _toInt(Object? v, [int fallback = 0]) {
  if (v is int) return v;
  if (v is num) return v.round();
  if (v is String) return int.tryParse(v) ?? double.tryParse(v)?.round() ?? fallback;
  return fallback;
}

bool _toBool(Object? v) {
  if (v is bool) return v;
  if (v is num) return v != 0;
  if (v is String) return v.toLowerCase() == 'true' || v == '1';
  return false;
}

/// Parses ISO-8601 strings and epoch numbers (seconds or milliseconds).
DateTime _toDateTime(Object? v) {
  if (v is String) {
    final parsed = DateTime.tryParse(v);
    if (parsed != null) return parsed;
    final asNum = num.tryParse(v);
    if (asNum != null) return _fromEpoch(asNum);
  }
  if (v is num) return _fromEpoch(v);
  return DateTime.now();
}

DateTime _fromEpoch(num v) {
  // Anything below ~year 2286 in seconds is < 1e10; treat larger as millis.
  final millis = v.abs() < 1e10 ? (v * 1000).round() : v.round();
  return DateTime.fromMillisecondsSinceEpoch(millis, isUtc: true);
}

enum DriverStatus {
  idle('Idle'),
  toPickup('To pickup'),
  onTrip('On trip'),
  offline('Offline'),
  incident('Incident'),
  unknown('Unknown');

  const DriverStatus(this.label);
  final String label;

  static DriverStatus fromWire(Object? raw) {
    final s = raw?.toString().toLowerCase().replaceAll('-', '_') ?? '';
    switch (s) {
      case 'idle':
      case 'available':
      case 'online':
        return DriverStatus.idle;
      case 'to_pickup':
      case 'en_route':
      case 'enroute':
      case 'dispatched':
      case 'assigned':
      case 'pickup':
        return DriverStatus.toPickup;
      case 'on_trip':
      case 'in_trip':
      case 'busy':
      case 'on_ride':
      case 'in_ride':
        return DriverStatus.onTrip;
      case 'offline':
        return DriverStatus.offline;
      case 'incident':
      case 'suspended':
      case 'sos':
      case 'alert':
        return DriverStatus.incident;
      default:
        return DriverStatus.unknown;
    }
  }

  String get wire => switch (this) {
        DriverStatus.idle => 'idle',
        DriverStatus.toPickup => 'to_pickup',
        DriverStatus.onTrip => 'on_trip',
        DriverStatus.offline => 'offline',
        DriverStatus.incident => 'incident',
        DriverStatus.unknown => 'unknown',
      };
}

class DriverPosition {
  const DriverPosition({
    required this.id,
    required this.lat,
    required this.lng,
    this.heading = 0,
    this.speedKmh = 0,
    this.status = DriverStatus.unknown,
    this.tier = 'standard',
    this.battery,
  });

  final String id;
  final double lat;
  final double lng;

  /// Degrees clockwise from north.
  final double heading;
  final double speedKmh;
  final DriverStatus status;
  final String tier;

  /// Battery level in percent (0-100), null for ICE vehicles.
  final double? battery;

  /// Autonomous vehicles are flagged by tier (`av`, `autonomous`, `robotaxi`).
  bool get isAutonomous {
    final t = tier.toLowerCase();
    return t == 'av' || t == 'autonomous' || t == 'robotaxi' || t.startsWith('av_');
  }

  factory DriverPosition.fromJson(Map<String, dynamic> json) => DriverPosition(
        id: json['id'].toString(),
        lat: _toDouble(json['lat']),
        lng: _toDouble(json['lng']),
        heading: _toDouble(json['heading']),
        speedKmh: _toDouble(json['speed_kmh']),
        status: DriverStatus.fromWire(json['status']),
        tier: json['tier']?.toString() ?? 'standard',
        battery: _toNullableDouble(json['battery']),
      );

  Map<String, dynamic> toJson() => {
        'id': id,
        'lat': lat,
        'lng': lng,
        'heading': heading,
        'speed_kmh': speedKmh,
        'status': status.wire,
        'tier': tier,
        'battery': battery,
      };
}

/// Feed filter buckets.
enum FeedCategory { match, unmatched, incident, other }

class AgentEvent {
  AgentEvent({
    required this.agent,
    required this.decision,
    required this.reasoning,
    required this.confidence,
    required this.latencyMs,
    required this.usedFallback,
    required this.ts,
    String? id,
    this.kind = '',
    this.rideId = '',
    this.driverId = '',
    this.tier = '',
    this.requestedTier = '',
    this.upgraded = false,
  }) : id = id ?? '${ts.microsecondsSinceEpoch}-$agent-${decision.hashCode}';

  /// `match`, `no_match`, `incident`, `scenario` (empty from older servers).
  final String kind;

  /// Full ride UUID and driver id, when the event concerns one.
  final String rideId;
  final String driverId;

  /// Tier of the car serving the ride, and the tier the rider asked for.
  final String tier;
  final String requestedTier;

  /// A free tier upgrade was applied.
  final bool upgraded;

  /// Which feed filter this event belongs to. Uses [kind], falling back to
  /// the decision text for servers that do not send it.
  FeedCategory get category {
    switch (kind) {
      case 'match':
        return FeedCategory.match;
      case 'no_match':
        return FeedCategory.unmatched;
      case 'incident':
        return FeedCategory.incident;
      case '':
        break;
      default:
        return FeedCategory.other;
    }
    final d = decision.toLowerCase();
    final a = agent.toLowerCase().replaceAll(RegExp(r'[^a-z]'), '');
    if (a.startsWith('safetysentinel')) return FeedCategory.incident;
    if (d.startsWith('no driver')) return FeedCategory.unmatched;
    if (d.startsWith('matched')) return FeedCategory.match;
    return FeedCategory.other;
  }

  final String id;
  final String agent;
  final String decision;
  final String reasoning;

  /// 0..1
  final double confidence;
  final double latencyMs;
  final bool usedFallback;
  final DateTime ts;

  factory AgentEvent.fromJson(Map<String, dynamic> json) {
    var confidence = _toDouble(json['confidence']);
    // Accept percentages too.
    if (confidence > 1) confidence = confidence / 100;
    return AgentEvent(
      agent: json['agent']?.toString() ?? 'unknown',
      decision: json['decision']?.toString() ?? '',
      reasoning: json['reasoning']?.toString() ?? '',
      confidence: confidence.clamp(0, 1).toDouble(),
      latencyMs: _toDouble(json['latency_ms']),
      usedFallback: _toBool(json['used_fallback']),
      ts: _toDateTime(json['ts']),
      kind: json['kind']?.toString() ?? '',
      rideId: json['ride_id']?.toString() ?? '',
      driverId: json['driver_id']?.toString() ?? '',
      tier: json['tier']?.toString() ?? '',
      requestedTier: json['requested_tier']?.toString() ?? '',
      upgraded: _toBool(json['upgraded']),
    );
  }

  Map<String, dynamic> toJson() => {
        'type': 'agent_event',
        'agent': agent,
        'decision': decision,
        'reasoning': reasoning,
        'confidence': confidence,
        'latency_ms': latencyMs,
        'used_fallback': usedFallback,
        'ts': ts.toUtc().toIso8601String(),
        if (kind.isNotEmpty) 'kind': kind,
        if (rideId.isNotEmpty) 'ride_id': rideId,
        if (driverId.isNotEmpty) 'driver_id': driverId,
        if (tier.isNotEmpty) 'tier': tier,
        if (requestedTier.isNotEmpty) 'requested_tier': requestedTier,
        if (upgraded) 'upgraded': true,
      };
}

class DispatchMetrics {
  const DispatchMetrics({
    required this.p50Ms,
    required this.p95Ms,
    required this.p99Ms,
    required this.matchesTotal,
    required this.activeDrivers,
    required this.idleDrivers,
    required this.openRequests,
    required this.ridesInProgress,
  });

  final double p50Ms;
  final double p95Ms;
  final double p99Ms;
  final int matchesTotal;
  final int activeDrivers;
  final int idleDrivers;
  final int openRequests;
  final int ridesInProgress;

  factory DispatchMetrics.fromJson(Map<String, dynamic> json) => DispatchMetrics(
        p50Ms: _toDouble(json['p50_ms']),
        p95Ms: _toDouble(json['p95_ms']),
        p99Ms: _toDouble(json['p99_ms']),
        matchesTotal: _toInt(json['matches_total']),
        activeDrivers: _toInt(json['active_drivers']),
        idleDrivers: _toInt(json['idle_drivers']),
        openRequests: _toInt(json['open_requests']),
        ridesInProgress: _toInt(json['rides_in_progress']),
      );

  Map<String, dynamic> toJson() => {
        'p50_ms': p50Ms,
        'p95_ms': p95Ms,
        'p99_ms': p99Ms,
        'matches_total': matchesTotal,
        'active_drivers': activeDrivers,
        'idle_drivers': idleDrivers,
        'open_requests': openRequests,
        'rides_in_progress': ridesInProgress,
      };
}

/// A decoded WebSocket frame from `/v1/stream`.
sealed class OpsMessage {
  const OpsMessage();
}

class DriversMessage extends OpsMessage {
  const DriversMessage(this.drivers);
  final List<DriverPosition> drivers;
}

class AgentEventMessage extends OpsMessage {
  const AgentEventMessage(this.event);
  final AgentEvent event;
}

class MetricsMessage extends OpsMessage {
  const MetricsMessage(this.metrics);
  final DispatchMetrics metrics;
}

/// Parses one frame (a JSON string or an already-decoded map).
/// Returns null for malformed frames or unknown topics/types.
OpsMessage? parseOpsMessage(Object? raw) {
  try {
    final Object? decoded = raw is String ? jsonDecode(raw) : raw;
    if (decoded is! Map) return null;
    final topic = decoded['topic'];
    final data = decoded['data'];
    if (topic == 'drivers') {
      if (data is! List) return null;
      final drivers = <DriverPosition>[];
      for (final d in data) {
        if (d is Map && d['id'] != null) {
          drivers.add(DriverPosition.fromJson(Map<String, dynamic>.from(d)));
        }
      }
      return DriversMessage(drivers);
    }
    if (topic == 'ops') {
      if (data is! Map) return null;
      final map = Map<String, dynamic>.from(data);
      switch (map['type']) {
        case 'agent_event':
          return AgentEventMessage(AgentEvent.fromJson(map));
        case 'metrics':
          return MetricsMessage(DispatchMetrics.fromJson(map));
      }
    }
    return null;
  } on FormatException {
    return null;
  } on TypeError {
    return null;
  }
}

/// What-if scenarios the simulator can inject.
enum Scenario {
  rushHour('rush_hour', 'Rush hour'),
  rainStorm('rain_storm', 'Rain storm'),
  concertExit('concert_exit', 'Concert exit'),
  safetyIncident('safety_incident', 'Safety incident');

  const Scenario(this.wire, this.label);
  final String wire;
  final String label;
}

/// Connection lifecycle as shown to the operator.
enum OpsConnectionStatus {
  /// First connection attempt, backend may be cold-starting.
  waking,

  /// Streaming.
  live,

  /// Lost an established connection, retrying with backoff.
  reconnecting,
}
