import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/ops/models/ops_models.dart';
import 'package:hexago_app/features/ops/widgets/metrics_strip.dart';
import 'package:hexago_app/theme/hexago_theme.dart';

import 'fakes.dart';

Widget _host(DispatchMetrics? m, {double width = 1280}) => MaterialApp(
      home: Scaffold(
        body: Center(child: SizedBox(width: width, child: MetricsStrip(metrics: m))),
      ),
    );

Text _value(WidgetTester tester, String id) => tester.widget<Text>(find.byKey(ValueKey('metric-value-$id')));

void main() {
  testWidgets('shows every KPI with formatted values', (tester) async {
    await tester.pumpWidget(_host(sampleMetrics));

    for (final label in [
      'Active drivers', 'Idle', 'Open requests', 'Rides in progress', 'Match p50', 'Match p99', 'Total matches',
    ]) {
      expect(find.text(label), findsOneWidget, reason: label);
    }
    expect(_value(tester, 'active').data, '287');
    expect(_value(tester, 'idle').data, '96');
    expect(_value(tester, 'open').data, '41');
    expect(_value(tester, 'rides').data, '152');
    expect(_value(tester, 'p50').data, '23.40 ms');
    expect(_value(tester, 'p99').data, '87.20 ms');
    expect(_value(tester, 'matches').data, '12,345');
  });

  testWidgets('match latency under 50 ms is green, otherwise not', (tester) async {
    await tester.pumpWidget(_host(sampleMetrics));
    expect(_value(tester, 'p50').style!.color, HexaColors.good);
    expect(_value(tester, 'p99').style!.color, isNot(HexaColors.good));
    expect(_value(tester, 'p99').style!.color, HexaColors.warn);
  });

  testWidgets('49.9 ms is green, 50 ms is not, 200 ms is red', (tester) async {
    DispatchMetrics with50(double p50, double p99) => DispatchMetrics(
          p50Ms: p50, p95Ms: 0, p99Ms: p99, matchesTotal: 0,
          activeDrivers: 0, idleDrivers: 0, openRequests: 0, ridesInProgress: 0,
        );
    await tester.pumpWidget(_host(with50(49.9, 50)));
    expect(_value(tester, 'p50').style!.color, HexaColors.good);
    expect(_value(tester, 'p99').style!.color, HexaColors.warn);

    await tester.pumpWidget(_host(with50(5, 200)));
    expect(_value(tester, 'p50').data, '5.00 ms');
    expect(_value(tester, 'p99').style!.color, HexaColors.bad);
  });

  testWidgets('shows placeholders before the first sample', (tester) async {
    await tester.pumpWidget(_host(null));
    expect(find.text('—'), findsNWidgets(7));
    expect(_value(tester, 'p50').style!.color, Colors.white);
  });

  testWidgets('wraps without overflow on a phone-width screen', (tester) async {
    await tester.pumpWidget(_host(sampleMetrics, width: 360));
    expect(tester.takeException(), isNull);
    expect(find.byKey(const ValueKey('metric-matches')), findsOneWidget);
  });
}
