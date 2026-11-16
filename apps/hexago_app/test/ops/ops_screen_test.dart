import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/ops/models/ops_models.dart';
import 'package:hexago_app/features/ops/ops_command_center_screen.dart';
import 'package:hexago_app/features/ops/state/ops_providers.dart';

import 'fakes.dart';

Future<void> _pumpScreen(WidgetTester tester, FakeOpsSource source, {Size size = const Size(1400, 900)}) async {
  tester.view.physicalSize = size;
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.reset);
  await tester.pumpWidget(ProviderScope(
    overrides: [
      opsSourceProvider.overrideWith((ref) {
        source.start();
        return source;
      }),
      showMapTilesProvider.overrideWithValue(false),
    ],
    child: const MaterialApp(home: OpsCommandCenterScreen()),
  ));
  await tester.pump();
}

void main() {
  testWidgets('wires metrics, feed, banner and scenarios to the source', (tester) async {
    final source = FakeOpsSource(initialMetrics: sampleMetrics);
    await _pumpScreen(tester, source);

    expect(find.text('0 humans in the loop · 12,345 autonomous decisions today'), findsOneWidget);
    expect(find.text('FAKE'), findsOneWidget);
    expect(find.text('Rides in progress'), findsOneWidget);
    expect(find.byKey(const ValueKey('fleet-layer')), findsOneWidget);

    source.eventsCtrl.add(sampleEvent(id: 'e1'));
    source.eventsCtrl.add(sampleEvent(id: 'e2', decision: 'Second decision', usedFallback: true));
    await tester.pump(const Duration(milliseconds: 300));
    expect(find.text('Second decision'), findsOneWidget);
    expect(find.text('FALLBACK'), findsOneWidget);
    expect(find.text('0 humans in the loop · 12,347 autonomous decisions today'), findsOneWidget);

    source.driversCtrl.add([const DriverPosition(id: 'd1', lat: 12.97, lng: 77.59, status: DriverStatus.onTrip)]);
    await tester.pump(const Duration(milliseconds: 16));

    await tester.tap(find.byKey(const ValueKey('scenario-rain_storm')));
    await tester.pump();
    expect(source.triggered, [Scenario.rainStorm]);
    expect(find.textContaining('Rain storm injected'), findsOneWidget);

    await tester.pumpWidget(const SizedBox());
    expect(source.disposed, isFalse, reason: 'override owns lifecycle in tests');
  });

  testWidgets('shows "Waking up the fleet..." until the backend answers', (tester) async {
    final source = FakeOpsSource(initialStatus: OpsConnectionStatus.waking);
    await _pumpScreen(tester, source);
    expect(find.text('Waking up the fleet...'), findsOneWidget);
    expect(find.text('WAKING'), findsOneWidget);

    source.statusCtrl.add(OpsConnectionStatus.live);
    source.driversCtrl.add([const DriverPosition(id: 'd1', lat: 12.97, lng: 77.59)]);
    await tester.pump(const Duration(milliseconds: 50));
    expect(find.text('Waking up the fleet...'), findsNothing);
    expect(find.text('FAKE'), findsOneWidget);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('stacks vertically on a phone without overflow', (tester) async {
    final source = FakeOpsSource(initialMetrics: sampleMetrics);
    await _pumpScreen(tester, source, size: const Size(390, 844));
    expect(tester.takeException(), isNull);
    expect(find.byKey(const ValueKey('zero-humans-banner')), findsOneWidget);
    expect(find.byKey(const ValueKey('scenario-rush_hour')), findsOneWidget);
    await tester.pumpWidget(const SizedBox());
  });
}
