import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/ops/state/ops_providers.dart';
import 'package:hexago_app/features/safety/models/safety_models.dart';
import 'package:hexago_app/features/safety/state/safety_providers.dart';
import 'package:hexago_app/features/safety/widgets/incident_panel.dart';

import 'safety_fakes.dart';

Widget _wrap(Widget child, {FakeSafetyApi? api}) => ProviderScope(
      overrides: [
        showMapTilesProvider.overrideWithValue(false),
        if (api != null) safetyApiProvider.overrideWithValue(api),
        voiceProvider.overrideWith((ref) => FakeVoice()),
      ],
      child: MaterialApp(home: Scaffold(body: child)),
    );

void _size(WidgetTester tester, Size s) {
  tester.view.physicalSize = s;
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.reset);
}

void main() {
  testWidgets('timeline renders every step in order with actor and failures', (tester) async {
    await tester.pumpWidget(_wrap(SingleChildScrollView(child: IncidentTimeline(entries: sampleDetail.timeline))));
    final texts = ['safety signal on ride', 'calling the rider', 'no reply within 15 s: escalate', 'contact notified failed'];
    final ys = [for (final t in texts) tester.getTopLeft(find.text(t)).dy];
    expect(ys, orderedEquals([...ys]..sort()));
    expect(find.textContaining('safety detector'), findsOneWidget);
    expect(find.textContaining('safety sentinel'), findsNWidgets(3));
    final failed = tester.widget<Text>(find.text('contact notified failed'));
    expect(failed.style?.color, isNot(equals(null)));
    for (var i = 0; i < texts.length; i++) {
      expect(find.byKey(ValueKey('timeline-${sampleDetail.timeline[i].step}-$i')), findsOneWidget);
    }
    await tester.pumpWidget(_wrap(const IncidentTimeline(entries: [])));
    expect(find.text('No steps yet.'), findsOneWidget);
  });

  testWidgets('detail shows actions, transcript, timeline and the path replay', (tester) async {
    _size(tester, const Size(800, 1600));
    var closed = false;
    await tester.pumpWidget(_wrap(IncidentDetailView(detail: sampleDetail, onClose: () => closed = true)));
    await tester.pump();
    expect(find.text('ESCALATED'), findsOneWidget);
    expect(find.text('Driver frozen'), findsOneWidget);
    expect(find.text('Emergency notified · EMS-1'), findsOneWidget);
    expect(find.text('Location shared'), findsOneWidget);
    expect(find.text('Contact notified'), findsOneWidget);
    expect(find.text('Replacement AV dispatched · #ride-2'), findsOneWidget);
    expect(find.textContaining('https://gw/track/tok'), findsOneWidget);
    expect(find.text('REASONING (LLM)'), findsOneWidget);
    expect(find.text(sampleQuestion), findsOneWidget);
    expect(find.text('No answer within 15 s.'), findsOneWidget);
    expect(find.byKey(const ValueKey('incident-timeline')), findsOneWidget);
    expect(find.byKey(const ValueKey('path-replay')), findsOneWidget);
    expect(find.text('deviation in red'), findsOneWidget);
    expect(find.text('0 km/h'), findsOneWidget); // replay starts at the last point

    await tester.tap(find.byKey(const ValueKey('replay-play')));
    await tester.pump();
    expect(find.text('30 km/h'), findsOneWidget);
    await tester.pump(const Duration(milliseconds: 160));
    expect(find.text('35 km/h'), findsOneWidget);
    await tester.tap(find.byKey(const ValueKey('replay-play'))); // pause
    await tester.pump();
    await tester.tap(find.byKey(const ValueKey('replay-play'))); // restart and run to the end
    for (var i = 0; i < 5; i++) {
      await tester.pump(const Duration(milliseconds: 160));
    }
    expect(find.text('0 km/h'), findsOneWidget);
    await tester.tap(find.byIcon(Icons.close));
    expect(closed, isTrue);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('a trip that stayed on route has no red segment', (tester) async {
    const detail = IncidentDetail(
      summary: IncidentSummary(id: 'inc_x', status: 'monitoring', severity: 0.3),
      plannedRoute: [PathPoint(12.9716, 77.5946), PathPoint(12.9716, 77.6146)],
      vehiclePath: [PathPoint(12.9716, 77.5946), PathPoint(12.9717, 77.6000)],
    );
    _size(tester, const Size(800, 1400));
    await tester.pumpWidget(_wrap(const IncidentDetailView(detail: detail)));
    await tester.pump();
    expect(find.text('on route'), findsOneWidget);
    expect(find.text('No call was placed.'), findsOneWidget);
    expect(find.text('MONITORING'), findsOneWidget);
    await tester.pumpWidget(_wrap(const SizedBox(height: 200, child: PathReplay(plan: [], path: []))));
    expect(find.text('No vehicle path recorded.'), findsOneWidget);
  });

  test('deviation runs split the path at 300 m', () {
    const plan = [PathPoint(12.9716, 77.5946), PathPoint(12.9716, 77.6146)];
    const path = [
      PathPoint(12.9716, 77.5946),
      PathPoint(12.9716, 77.6000),
      PathPoint(12.9760, 77.6030), // ~490 m north of the route
      PathPoint(12.9770, 77.6040),
      PathPoint(12.9717, 77.6100),
    ];
    expect(deviationMeters(path[2], plan), closeTo(487, 5));
    expect(deviationMeters(path[0], const [PathPoint(12.9726, 77.5946)]), closeTo(110.6, 1));
    expect(deviationMeters(path[0], const []), 0);
    final runs = deviationRuns(path, plan, 300);
    expect(runs.map((r) => r.off), [false, true, false]);
    expect(runs[1].points.first, path[1], reason: 'runs share their boundary point');
    expect(runs[2].points.length, 2);
    expect(deviationRuns(path, const [], 300).single.off, isFalse);
  });

  testWidgets('panel lists incidents, opens the rider phone and the detail', (tester) async {
    _size(tester, const Size(1200, 1000));
    final api = FakeSafetyApi(
      list: IncidentList(incidents: [
        const IncidentSummary(id: 'inc_abc123', status: 'check_in', severity: 0.46, rideId: 'ride-1234567', driverId: 'd_007',
            reason: 'severity 0.46: calling the rider', pendingCheckinId: 'chk_1'),
        IncidentSummary(id: 'inc_def', status: 'escalated', severity: 0.9, driverId: 'd_002', reason: 'severity 0.90',
            createdAt: DateTime.utc(2026, 11, 18, 6, 30)),
      ], pendingCalls: [
        sampleCall()
      ], suppressed: 0),
      detail: sampleDetail,
    );
    await tester.pumpWidget(_wrap(const SizedBox(width: 420, height: 800, child: IncidentPanel(pollEvery: Duration(seconds: 1))), api: api));
    await tester.pump();
    expect(find.text('Safety incidents'), findsOneWidget);
    expect(find.text('CHECKING IN'), findsOneWidget);
    expect(find.text('ESCALATED'), findsOneWidget);
    expect(find.text('0.46'), findsOneWidget);
    expect(find.byKey(const ValueKey('live-checkin-chk_1')), findsOneWidget);
    expect(find.textContaining('15 s left before escalation'), findsOneWidget);

    await tester.tap(find.byKey(const ValueKey('open-rider-phone-chk_1')));
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 300));
    expect(find.byKey(const ValueKey('rider-phone-dialog')), findsOneWidget);
    expect(find.text(sampleQuestion), findsOneWidget);
    await tester.tap(find.byKey(const ValueKey('quick-fine')));
    await tester.pump();
    await tester.pump();
    expect(api.replies, ["I'm fine"]);
    await tester.tap(find.byKey(const ValueKey('safety-call-close')));
    await tester.pump(const Duration(milliseconds: 300));

    await tester.tap(find.byKey(const ValueKey('incident-inc_def')));
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 300));
    expect(find.byKey(const ValueKey('incident-detail')), findsOneWidget);
    expect(find.text('Driver frozen'), findsOneWidget);
    await tester.pump(const Duration(seconds: 3)); // live refresh
    await tester.tap(find.byIcon(Icons.close).last);
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 300));
    expect(find.byKey(const ValueKey('incident-detail')), findsNothing);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('panel shows empty state, suppressed count and errors', (tester) async {
    final api = FakeSafetyApi(list: const IncidentList(suppressed: 2));
    await tester.pumpWidget(_wrap(const SizedBox(width: 420, height: 500, child: IncidentPanel()), api: api));
    await tester.pump();
    expect(find.textContaining('No incidents'), findsOneWidget);
    expect(find.text('2 suppressed'), findsOneWidget);
    await tester.pumpWidget(const SizedBox());

    final broken = _BrokenApi();
    await tester.pumpWidget(_wrap(const SizedBox(width: 420, height: 500, child: IncidentPanel()), api: broken));
    await tester.pump();
    expect(find.text('Safety service unavailable'), findsOneWidget);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('detail loader shows an error when the incident cannot load', (tester) async {
    await tester.pumpWidget(_wrap(const IncidentDetailLoader(incidentId: 'x'), api: _BrokenApi()));
    await tester.pump();
    expect(find.textContaining('Could not load the incident'), findsOneWidget);
    await tester.pumpWidget(const SizedBox());
  });

  test('colours and labels', () {
    expect(severityColor(0.9), isNot(severityColor(0.5)));
    expect(severityColor(0.1), isNot(severityColor(0.5)));
    expect(statusLabel('check_in'), 'CHECKING IN');
    expect(statusLabel(''), 'UNKNOWN');
    for (final s in ['escalated', 'check_in', 'monitoring', 'resolved', 'other']) {
      expect(statusColorFor(s), isA<Color>());
    }
    for (final a in ['rider', 'ops_supervisor', 'safety_detector', 'safety_sentinel']) {
      expect(IncidentTimeline.actorColor(a), isA<Color>());
    }
  });
}

class _BrokenApi extends FakeSafetyApi {
  @override
  Future<IncidentList> listIncidents() async => throw Exception('down');
  @override
  Future<IncidentDetail> getIncident(String id) async => throw Exception('down');
}
