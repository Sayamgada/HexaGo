import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';

import 'package:hexago_app/config/app_config.dart';
import 'package:hexago_app/features/ops/state/ops_providers.dart';
import 'package:hexago_app/main.dart';

import 'ops/fakes.dart';

Widget _app() => ProviderScope(
      overrides: [
        opsSourceProvider.overrideWith((ref) => FakeOpsSource()),
        showMapTilesProvider.overrideWithValue(false),
      ],
      child: const HexaGoApp(),
    );

void main() {
  group('AppConfig Tests', () {
    test('default configuration uses localhost:8080', () {
      expect(AppConfig.apiBaseUrl, equals('http://localhost:8080'));
      expect(AppConfig.wsBaseUrl, equals('ws://localhost:8080'));
      expect(AppConfig.appName, equals('HexaGo'));
    });
  });

  group('HexaGo Home Screen Widget Tests', () {
    testWidgets('renders HexaGo branding and all three role cards', (WidgetTester tester) async {
      await tester.pumpWidget(_app());

      // Verify HexaGo brand header
      expect(find.text('HexaGo'), findsWidgets);
      expect(find.textContaining('AI-Native Autonomous Mobility'), findsOneWidget);

      // Verify three role cards exist
      expect(find.byKey(const Key('role-card-Rider')), findsOneWidget);
      expect(find.byKey(const Key('role-card-Driver')), findsOneWidget);
      expect(find.byKey(const Key('role-card-Ops Command Center')), findsOneWidget);

      // Verify role descriptions
      expect(find.textContaining('Natural language intent'), findsOneWidget);
      expect(find.textContaining('H3 spatial telemetry'), findsOneWidget);
      expect(find.textContaining('Live agent audit events'), findsOneWidget);
    });

    testWidgets('tapping on Rider role card updates selection', (WidgetTester tester) async {
      await tester.pumpWidget(_app());

      final riderCardFinder = find.byKey(const Key('role-card-Rider'));
      expect(riderCardFinder, findsOneWidget);

      await tester.tap(riderCardFinder);
      await tester.pumpAndSettle();

      final badgeFinder = find.byKey(const Key('selected-role-badge'));
      expect(badgeFinder, findsOneWidget);
      expect(find.descendant(of: badgeFinder, matching: find.text('Selected Role: Rider')), findsOneWidget);
    });

    testWidgets('tapping the Ops Command Center card opens the command center', (WidgetTester tester) async {
      tester.view.physicalSize = const Size(1400, 900);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      await tester.pumpWidget(_app());

      await tester.tap(find.byKey(const Key('role-card-Ops Command Center')));
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 500));

      expect(find.byKey(const ValueKey('ops-command-center')), findsOneWidget);
      expect(find.textContaining('0 humans in the loop'), findsOneWidget);
      await tester.pumpWidget(const SizedBox());
    });
  });
}
