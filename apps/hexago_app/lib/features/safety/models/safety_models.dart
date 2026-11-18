/// Wire models for HexaGo Safety (the Safety Sentinel behind the gateway):
///
/// * `GET  /v1/safety/checkins/{id}`          -> [SafetyCall]
/// * `POST /v1/safety/checkins/{id}/reply`    -> [ReplyResult]
/// * `GET  /v1/safety/incidents`              -> [IncidentList]
/// * `GET  /v1/safety/incidents/{id}`         -> [IncidentDetail]
///
/// Parsing is tolerant like the Ops models: missing fields take defaults.
library;

double _d(Object? v, [double fallback = 0]) {
  if (v is num) return v.toDouble();
  if (v is String) return double.tryParse(v) ?? fallback;
  return fallback;
}

String _s(Object? v) => v == null ? '' : v.toString();

DateTime? _t(Object? v) => v is String ? DateTime.tryParse(v) : null;

Map<String, dynamic> _m(Object? v) => v is Map ? Map<String, dynamic>.from(v) : <String, dynamic>{};

List<Map<String, dynamic>> _l(Object? v) =>
    v is List ? [for (final e in v) if (e is Map) Map<String, dynamic>.from(e)] : const [];

/// One rider check-in call.
class SafetyCall {
  const SafetyCall({
    required this.checkinId,
    required this.question,
    this.incidentId = '',
    this.rideId = '',
    this.driverId = '',
    this.timeoutS = 30,
    this.remainingS = 30,
    this.status = 'pending',
    this.incidentStatus = '',
    this.severity = 0,
    this.triggers = const [],
    this.transcript = '',
  });

  final String checkinId;
  final String incidentId;
  final String rideId;
  final String driverId;
  final String question;
  final double timeoutS;
  final double remainingS;
  final String status; // pending | answered | timed_out
  final String incidentStatus; // check_in | monitoring | escalated | resolved
  final double severity;
  final List<String> triggers;
  final String transcript;

  bool get pending => status == 'pending';

  factory SafetyCall.fromJson(Map<String, dynamic> j) => SafetyCall(
        checkinId: _s(j['checkin_id'] ?? j['id']),
        incidentId: _s(j['incident_id']),
        rideId: _s(j['ride_id']),
        driverId: _s(j['driver_id']),
        question: _s(j['question']),
        timeoutS: _d(j['timeout_s'], 30),
        remainingS: _d(j['remaining_s'], _d(j['timeout_s'], 30)),
        status: j['status'] == null ? 'pending' : _s(j['status']),
        incidentStatus: _s(j['incident_status']),
        severity: _d(j['severity']),
        triggers: [for (final t in (j['triggers'] is List ? j['triggers'] as List : const [])) _s(t is Map ? t['type'] : t)],
        transcript: _s(j['transcript']),
      );
}

/// What the sentinel did with the rider's answer.
class ReplyResult {
  const ReplyResult({required this.action, required this.label, this.incidentStatus = '', this.matched = const []});

  final String action; // monitor | escalate
  final String label; // fine | distress | unclear
  final String incidentStatus;
  final List<String> matched;

  bool get escalated => action == 'escalate';

  factory ReplyResult.fromJson(Map<String, dynamic> j) {
    final c = _m(j['classification']);
    return ReplyResult(
      action: _s(j['action']),
      label: _s(c['label']),
      incidentStatus: _s(j['incident_status']),
      matched: [for (final m in (c['matched'] is List ? c['matched'] as List : const [])) _s(m)],
    );
  }
}

/// The incident flags shown as "actions taken".
class IncidentFlags {
  const IncidentFlags({
    this.driverFrozen = false,
    this.emergencyNotified = false,
    this.locationShared = false,
    this.contactNotified = false,
  });

  final bool driverFrozen;
  final bool emergencyNotified;
  final bool locationShared;
  final bool contactNotified;

  factory IncidentFlags.fromJson(Map<String, dynamic> j) => IncidentFlags(
        driverFrozen: j['driver_frozen'] == true,
        emergencyNotified: j['emergency_notified'] == true,
        locationShared: j['location_shared'] == true,
        contactNotified: j['contact_notified'] == true,
      );
}

class IncidentSummary {
  const IncidentSummary({
    required this.id,
    required this.status,
    required this.severity,
    this.rideId = '',
    this.driverId = '',
    this.tier = '',
    this.decision = '',
    this.reason = '',
    this.triggers = const [],
    this.createdAt,
    this.pendingCheckinId,
    this.flags = const IncidentFlags(),
  });

  final String id;
  final String status;
  final double severity;
  final String rideId;
  final String driverId;
  final String tier;
  final String decision;
  final String reason;
  final List<String> triggers;
  final DateTime? createdAt;
  final String? pendingCheckinId;
  final IncidentFlags flags;

  factory IncidentSummary.fromJson(Map<String, dynamic> j) => IncidentSummary(
        id: _s(j['id']),
        status: _s(j['status']),
        severity: _d(j['severity']),
        rideId: _s(j['ride_id']),
        driverId: _s(j['driver_id']),
        tier: _s(j['tier']),
        decision: _s(j['decision']),
        reason: _s(j['reason']),
        triggers: [for (final t in _l(j['triggers'])) _s(t['type'])],
        createdAt: _t(j['created_at']),
        pendingCheckinId: j['pending_checkin_id'] == null ? null : _s(j['pending_checkin_id']),
        flags: IncidentFlags.fromJson(_m(j['flags'])),
      );
}

class IncidentList {
  const IncidentList({this.incidents = const [], this.pendingCalls = const [], this.suppressed = 0});

  final List<IncidentSummary> incidents;
  final List<SafetyCall> pendingCalls;
  final int suppressed;

  factory IncidentList.fromJson(Map<String, dynamic> j) => IncidentList(
        incidents: [for (final i in _l(j['incidents'])) IncidentSummary.fromJson(i)],
        pendingCalls: [for (final c in _l(j['pending_checkins'])) SafetyCall.fromJson(c)],
        suppressed: _l(j['suppressed']).length,
      );
}

class TimelineEntry {
  const TimelineEntry({required this.step, required this.actor, required this.summary, this.at, this.failed = false});

  final String step;
  final String actor;
  final String summary;
  final DateTime? at;
  final bool failed;

  factory TimelineEntry.fromJson(Map<String, dynamic> j) {
    final details = _m(j['details']);
    final step = _s(j['step']);
    return TimelineEntry(
      step: step,
      actor: _s(j['actor']),
      summary: _s(details['summary']).isEmpty ? step.replaceAll('_', ' ') : _s(details['summary']),
      at: _t(j['at']),
      failed: step.endsWith('_failed'),
    );
  }
}

class TranscriptLine {
  const TranscriptLine({required this.role, required this.text, this.at});

  final String role; // agent | rider | system
  final String text;
  final DateTime? at;

  factory TranscriptLine.fromJson(Map<String, dynamic> j) =>
      TranscriptLine(role: _s(j['role']), text: _s(j['text']), at: _t(j['at']));
}

class PathPoint {
  const PathPoint(this.lat, this.lng, {this.speedKmh = 0});

  final double lat;
  final double lng;
  final double speedKmh;
}

class IncidentDetail {
  const IncidentDetail({
    required this.summary,
    this.reasoning = '',
    this.reasoningSource = '',
    this.timeline = const [],
    this.transcript = const [],
    this.plannedRoute = const [],
    this.vehiclePath = const [],
    this.deviationM = 300,
    this.trackUrl,
    this.emergencyReference,
    this.replacementRideId,
    this.resolution,
  });

  final IncidentSummary summary;
  final String reasoning;
  final String reasoningSource;
  final List<TimelineEntry> timeline;
  final List<TranscriptLine> transcript;
  final List<PathPoint> plannedRoute;
  final List<PathPoint> vehiclePath;
  final double deviationM;
  final String? trackUrl;
  final String? emergencyReference;
  final String? replacementRideId;
  final String? resolution;

  factory IncidentDetail.fromJson(Map<String, dynamic> j) {
    final actions = _m(j['actions']);
    final summary = IncidentSummary.fromJson(j['summary'] is Map ? _m(j['summary']) : j);
    final planned = <PathPoint>[];
    for (final p in (j['planned_route'] is List ? j['planned_route'] as List : const [])) {
      if (p is List && p.length >= 2) planned.add(PathPoint(_d(p[0]), _d(p[1])));
      if (p is Map) planned.add(PathPoint(_d(p['lat']), _d(p['lng'])));
    }
    String? opt(Map<String, dynamic> m, String k) => _s(m[k]).isEmpty ? null : _s(m[k]);
    return IncidentDetail(
      summary: summary,
      reasoning: _s(j['reasoning']),
      reasoningSource: _s(j['reasoning_source']),
      timeline: [for (final e in _l(j['timeline'])) TimelineEntry.fromJson(e)],
      transcript: [for (final e in _l(j['transcript'])) TranscriptLine.fromJson(e)],
      plannedRoute: planned,
      vehiclePath: [
        for (final p in _l(j['vehicle_path']))
          if (p['lat'] != null && p['lng'] != null) PathPoint(_d(p['lat']), _d(p['lng']), speedKmh: _d(p['speed_kmh']))
      ],
      deviationM: _d(j['deviation_m'], 300),
      trackUrl: opt(_m(actions['location_shared']), 'url'),
      emergencyReference: opt(_m(actions['emergency_notified']), 'reference'),
      replacementRideId: opt(_m(actions['replacement_ride']), 'ride_id'),
      resolution: _s(j['resolution']).isEmpty ? null : _s(j['resolution']),
    );
  }
}
