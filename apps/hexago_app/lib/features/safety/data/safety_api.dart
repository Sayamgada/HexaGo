import 'dart:async';
import 'dart:convert';

import 'package:dio/dio.dart';

import '../models/safety_models.dart';

/// Thrown when a check-in no longer accepts replies (answered or timed out).
class CheckinClosedException implements Exception {
  CheckinClosedException(this.call);
  final SafetyCall call;
  @override
  String toString() => 'check-in is ${call.status}';
}

/// HexaGo Safety endpoints (through the dispatch-go gateway).
abstract class SafetyApi {
  Future<SafetyCall> getCheckin(String id);
  Future<ReplyResult> reply(String id, String transcript);
  Future<IncidentList> listIncidents();
  Future<IncidentDetail> getIncident(String id);
}

Map<String, dynamic> _json(Object? data) {
  final d = data is String ? jsonDecode(data) : data;
  return d is Map ? Map<String, dynamic>.from(d) : <String, dynamic>{};
}

class DioSafetyApi implements SafetyApi {
  DioSafetyApi({required String apiBaseUrl, Dio? dio})
      : _api = apiBaseUrl.endsWith('/') ? apiBaseUrl.substring(0, apiBaseUrl.length - 1) : apiBaseUrl,
        _dio = dio ??
            Dio(BaseOptions(connectTimeout: const Duration(seconds: 60), receiveTimeout: const Duration(seconds: 60)));

  final String _api;
  final Dio _dio;

  @override
  Future<SafetyCall> getCheckin(String id) async =>
      SafetyCall.fromJson(_json((await _dio.get<dynamic>('$_api/v1/safety/checkins/$id')).data));

  @override
  Future<ReplyResult> reply(String id, String transcript) async {
    try {
      final res = await _dio.post<dynamic>('$_api/v1/safety/checkins/$id/reply', data: {'transcript': transcript});
      return ReplyResult.fromJson(_json(res.data));
    } on DioException catch (e) {
      if (e.response?.statusCode == 409) throw CheckinClosedException(SafetyCall.fromJson(_json(e.response?.data)));
      rethrow;
    }
  }

  @override
  Future<IncidentList> listIncidents() async =>
      IncidentList.fromJson(_json((await _dio.get<dynamic>('$_api/v1/safety/incidents')).data));

  @override
  Future<IncidentDetail> getIncident(String id) async =>
      IncidentDetail.fromJson(_json((await _dio.get<dynamic>('$_api/v1/safety/incidents/$id')).data));
}

/// Offline demo backend (USE_MOCK): one live check-in that follows the real rules
/// ("I'm fine" -> monitor, distress or silence -> escalate).
class MockSafetyApi implements SafetyApi {
  MockSafetyApi({DateTime Function()? now}) : _now = now ?? DateTime.now {
    _created = _now();
  }

  final DateTime Function() _now;
  late DateTime _created;
  static const timeoutS = 15.0;
  static const question =
      'Hi, this is HexaGo Safety. We noticed your vehicle left the planned route and braked very hard. Are you okay?';
  String _status = 'pending';
  String _incidentStatus = 'check_in';
  String? _answer;

  static const _distress = ['help', 'scared', 'stop', 'police', 'unsafe', 'hurt', 'not ok', 'not okay', 'emergency'];
  static const _fine = ['fine', 'ok', 'okay', 'all good', 'safe', 'good'];

  void _expire() {
    if (_status == 'pending' && _now().difference(_created).inMilliseconds >= timeoutS * 1000) {
      _status = 'timed_out';
      _incidentStatus = 'escalated';
    }
  }

  /// Starts a fresh mock call.
  void reset() {
    _created = _now();
    _status = 'pending';
    _incidentStatus = 'check_in';
    _answer = null;
  }

  SafetyCall _call() {
    _expire();
    final left = timeoutS - _now().difference(_created).inMilliseconds / 1000;
    return SafetyCall(
      checkinId: 'chk_demo',
      incidentId: 'inc_demo',
      rideId: 'ride_demo',
      driverId: 'd_042',
      question: question,
      timeoutS: timeoutS,
      remainingS: left < 0 ? 0 : left,
      status: _status,
      incidentStatus: _incidentStatus,
      severity: 0.46,
      triggers: const ['route_deviation', 'harsh_deceleration'],
      transcript: _answer ?? '',
    );
  }

  @override
  Future<SafetyCall> getCheckin(String id) async => _call();

  @override
  Future<ReplyResult> reply(String id, String transcript) async {
    final call = _call();
    if (!call.pending) throw CheckinClosedException(call);
    final t = transcript.toLowerCase();
    final distress = _distress.where(t.contains).toList();
    final fine = _fine.where(t.contains).toList();
    final label = distress.isNotEmpty ? 'distress' : (fine.isNotEmpty ? 'fine' : 'unclear');
    _status = 'answered';
    _answer = transcript;
    _incidentStatus = label == 'fine' ? 'monitoring' : 'escalated';
    return ReplyResult(
      action: label == 'fine' ? 'monitor' : 'escalate',
      label: label,
      incidentStatus: _incidentStatus,
      matched: label == 'distress' ? distress : fine,
    );
  }

  IncidentSummary _summary() {
    final call = _call();
    final escalated = _incidentStatus == 'escalated';
    return IncidentSummary(
      id: 'inc_demo',
      status: _incidentStatus,
      severity: escalated ? 0.8 : 0.46,
      rideId: 'ride_demo',
      driverId: 'd_042',
      tier: 'sedan',
      decision: escalated ? 'escalate' : (call.pending ? 'check_in' : 'monitor'),
      reason: escalated ? 'no safe answer from the rider' : 'severity 0.46: calling the rider to check in',
      triggers: const ['route_deviation', 'harsh_deceleration'],
      createdAt: _created,
      pendingCheckinId: call.pending ? 'chk_demo' : null,
      flags: IncidentFlags(
          driverFrozen: escalated, emergencyNotified: escalated, locationShared: escalated, contactNotified: escalated),
    );
  }

  @override
  Future<IncidentList> listIncidents() async {
    final call = _call();
    return IncidentList(incidents: [_summary()], pendingCalls: call.pending ? [call] : const []);
  }

  @override
  Future<IncidentDetail> getIncident(String id) async {
    final s = _summary();
    final t0 = _created;
    final escalated = s.status == 'escalated';
    return IncidentDetail(
      summary: s,
      reasoning: 'Telemetry shows the vehicle left the planned route and braked very hard (signal severity 0.46).',
      reasoningSource: 'template',
      timeline: [
        TimelineEntry(step: 'signal_received', actor: 'safety_detector', summary: 'safety signal: vehicle left the route', at: t0),
        TimelineEntry(step: 'decision', actor: 'safety_sentinel', summary: 'check_in: calling the rider', at: t0),
        TimelineEntry(step: 'checkin_started', actor: 'safety_sentinel', summary: 'calling the rider (15 s)', at: t0),
        if (_answer != null) TimelineEntry(step: 'reply_received', actor: 'rider', summary: 'rider replied: $_answer', at: t0),
        if (escalated) ...[
          TimelineEntry(step: 'escalated', actor: 'safety_sentinel', summary: 'escalating incident', at: t0),
          TimelineEntry(step: 'emergency_dispatched', actor: 'safety_sentinel', summary: 'emergency services dispatched (simulated)', at: t0),
          TimelineEntry(step: 'location_shared', actor: 'safety_sentinel', summary: 'live location shared', at: t0),
          TimelineEntry(step: 'driver_suspended', actor: 'safety_sentinel', summary: 'driver d_042 suspended; vehicle stopped', at: t0),
          TimelineEntry(step: 'contact_notified', actor: 'safety_sentinel', summary: 'emergency contact notified (simulated)', at: t0),
        ],
      ],
      transcript: [
        TranscriptLine(role: 'agent', text: question, at: t0),
        if (_answer != null) TranscriptLine(role: 'rider', text: _answer!, at: t0),
      ],
      plannedRoute: const [PathPoint(12.9716, 77.5946), PathPoint(12.9716, 77.6146)],
      vehiclePath: const [
        PathPoint(12.9716, 77.5946, speedKmh: 30),
        PathPoint(12.9716, 77.6000, speedKmh: 35),
        PathPoint(12.9750, 77.6030, speedKmh: 50),
        PathPoint(12.9760, 77.6040, speedKmh: 0),
      ],
      trackUrl: escalated ? 'https://example.invalid/track/demo' : null,
      emergencyReference: escalated ? 'EMS-DEMO' : null,
    );
  }
}
