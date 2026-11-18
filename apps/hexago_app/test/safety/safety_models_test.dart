import 'package:dio/dio.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/safety/data/safety_api.dart';
import 'package:hexago_app/features/safety/models/safety_models.dart';

void main() {
  test('parses a check-in, a reply and the incident list', () {
    final c = SafetyCall.fromJson({
      'type': 'safety_call', 'checkin_id': 'chk_1', 'incident_id': 'inc_1', 'ride_id': 'r1', 'driver_id': 'd_1',
      'question': 'Are you okay?', 'timeout_s': 15, 'remaining_s': '9.5', 'severity': 0.46,
      'triggers': ['route_deviation', {'type': 'harsh_deceleration'}], 'incident_status': 'check_in',
    });
    expect(c.pending, isTrue);
    expect(c.remainingS, 9.5);
    expect(c.triggers, ['route_deviation', 'harsh_deceleration']);
    expect(SafetyCall.fromJson({'id': 'x', 'timeout_s': 30}).remainingS, 30);

    final r = ReplyResult.fromJson({'action': 'escalate', 'incident_status': 'escalated',
      'classification': {'label': 'distress', 'matched': ['help']}});
    expect(r.escalated, isTrue);
    expect(r.matched, ['help']);

    final l = IncidentList.fromJson({
      'incidents': [
        {'id': 'inc_1', 'status': 'escalated', 'severity': 0.9, 'triggers': [{'type': 'route_deviation'}],
         'created_at': '2026-11-18T06:30:00Z', 'pending_checkin_id': null,
         'flags': {'driver_frozen': true, 'emergency_notified': true}},
        'junk',
      ],
      'pending_checkins': [{'checkin_id': 'chk_1', 'question': 'q'}],
      'suppressed': [{}, {}],
    });
    expect(l.incidents.single.flags.driverFrozen, isTrue);
    expect(l.incidents.single.flags.locationShared, isFalse);
    expect(l.incidents.single.createdAt, DateTime.utc(2026, 11, 18, 6, 30));
    expect(l.pendingCalls.single.checkinId, 'chk_1');
    expect(l.suppressed, 2);
  });

  test('parses an incident detail with timeline, transcript and paths', () {
    final d = IncidentDetail.fromJson({
      'id': 'inc_1', 'status': 'escalated', 'severity': 0.8, 'reasoning': 'r', 'reasoning_source': 'llm',
      'summary': {'id': 'inc_1', 'status': 'escalated', 'severity': 0.8, 'flags': {'location_shared': true}},
      'timeline': [
        {'step': 'signal_received', 'actor': 'safety_detector', 'at': '2026-11-18T06:30:00Z', 'details': {'summary': 'signal'}},
        {'step': 'driver_suspend_failed', 'actor': 'safety_sentinel', 'details': {}},
      ],
      'transcript': [{'role': 'agent', 'text': 'Are you okay?'}],
      'planned_route': [[12.97, 77.59], {'lat': 12.98, 'lng': 77.6}, 'bad'],
      'vehicle_path': [{'lat': 12.97, 'lng': 77.59, 'speed_kmh': 30}, {'lat': null}],
      'deviation_m': 300,
      'actions': {
        'location_shared': {'status': 'ok', 'url': 'https://gw/track/t'},
        'emergency_notified': {'status': 'ok', 'reference': 'EMS-1'},
        'replacement_ride': {'ride_id': 'r2'},
      },
      'resolution': 'all good',
    });
    expect(d.summary.flags.locationShared, isTrue);
    expect(d.timeline.first.summary, 'signal');
    expect(d.timeline.last.summary, 'driver suspend failed');
    expect(d.timeline.last.failed, isTrue);
    expect(d.plannedRoute.length, 2);
    expect(d.vehiclePath.single.speedKmh, 30);
    expect(d.trackUrl, 'https://gw/track/t');
    expect(d.emergencyReference, 'EMS-1');
    expect(d.replacementRideId, 'r2');
    expect(d.resolution, 'all good');
    final bare = IncidentDetail.fromJson({'id': 'x'});
    expect(bare.trackUrl, isNull);
    expect(bare.resolution, isNull);
    expect(bare.summary.id, 'x');
  });

  test('mock API follows the real rules', () async {
    var now = DateTime.utc(2026, 11, 18, 6, 30);
    final api = MockSafetyApi(now: () => now);
    var call = await api.getCheckin('chk_demo');
    expect(call.pending, isTrue);
    expect((await api.listIncidents()).pendingCalls, hasLength(1));
    final r = await api.reply('chk_demo', "I'm fine");
    expect(r.action, 'monitor');
    expect((await api.getIncident('inc_demo')).summary.status, 'monitoring');
    await expectLater(api.reply('chk_demo', 'help'), throwsA(isA<CheckinClosedException>()));

    api.reset();
    expect((await api.reply('chk_demo', 'help, I am scared')).action, 'escalate');
    api.reset();
    expect((await api.reply('chk_demo', 'banana')).label, 'unclear');

    api.reset();
    now = now.add(const Duration(seconds: 16));
    call = await api.getCheckin('chk_demo');
    expect(call.status, 'timed_out');
    expect(call.remainingS, 0);
    final det = await api.getIncident('inc_demo');
    expect(det.summary.flags.driverFrozen, isTrue);
    expect(det.timeline.map((e) => e.step), contains('driver_suspended'));
    expect(CheckinClosedException(call).toString(), contains('timed_out'));
  });

  test('Dio API maps endpoints and a 409 reply', () async {
    final dio = Dio();
    dio.httpClientAdapter = _Adapter();
    final api = DioSafetyApi(apiBaseUrl: 'http://gw/', dio: dio);
    expect((await api.getCheckin('chk_1')).question, 'q');
    expect((await api.listIncidents()).incidents.single.id, 'inc_1');
    expect((await api.getIncident('inc_1')).summary.status, 'escalated');
    expect((await api.reply('chk_1', 'fine')).action, 'monitor');
    await expectLater(api.reply('chk_closed', 'fine'), throwsA(isA<CheckinClosedException>()));
    await expectLater(api.reply('chk_boom', 'fine'), throwsA(isA<DioException>()));
  });
}

class _Adapter implements HttpClientAdapter {
  @override
  Future<ResponseBody> fetch(RequestOptions o, Stream<List<int>>? body, Future<void>? cancel) async {
    final p = o.uri.path;
    String json;
    var code = 200;
    if (p.endsWith('/v1/safety/checkins/chk_1')) {
      json = '{"checkin_id":"chk_1","question":"q"}';
    } else if (p.endsWith('/v1/safety/incidents')) {
      json = '{"incidents":[{"id":"inc_1","status":"check_in","severity":0.5}]}';
    } else if (p.endsWith('/v1/safety/incidents/inc_1')) {
      json = '{"id":"inc_1","status":"escalated","severity":0.9}';
    } else if (p.endsWith('/chk_1/reply')) {
      json = '{"action":"monitor","classification":{"label":"fine"}}';
    } else if (p.endsWith('/chk_closed/reply')) {
      code = 409;
      json = '{"error":"closed","checkin_id":"chk_closed","status":"timed_out"}';
    } else {
      code = 500;
      json = '{}';
    }
    return ResponseBody.fromString(json, code, headers: {
      Headers.contentTypeHeader: ['application/json'],
    });
  }

  @override
  void close({bool force = false}) {}
}
