import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';

import 'package:hexago_app/config/app_config.dart';
import 'package:hexago_app/features/ops/state/ops_providers.dart';
import 'package:hexago_app/features/rider/state/rider_providers.dart';
import 'package:hexago_app/features/safety/state/safety_providers.dart';
import 'package:hexago_app/features/safety/voice/voice_io.dart';
import 'package:hexago_app/main.dart';

import 'ops/fakes.dart';
import 'rider/rider_fakes.dart';

Widget _app() => ProviderScope(
      overrides: [
        opsSourceProvider.overrideWith((ref) => FakeOpsSource()),
        showMapTilesProvider.overrideWithValue(false),
        bookingApiProvider.overrideWithValue(FakeBookingApi()),
        voiceProvider.overrideWith((ref) => SilentVoiceIO()),
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
      expect(find.byKey(const Key('role-card-Developers')), findsOneWidget);

      // Verify role descriptions
      expect(find.textContaining('Natural language intent'), findsOneWidget);
      expect(find.textContaining('H3 spatial telemetry'), findsOneWidget);
      expect(find.textContaining('Live agent audit events'), findsOneWidget);
    });

    testWidgets('tapping on Rider role card opens the rider persona picker', (WidgetTester tester) async {
      await tester.pumpWidget(_app());

      final riderCardFinder = find.byKey(const Key('role-card-Rider'));
      expect(riderCardFinder, findsOneWidget);

      await tester.tap(riderCardFinder);
      await tester.pumpAndSettle();

      expect(find.text('HexaGo Rider'), findsOneWidget);
      expect(find.byKey(const Key('persona-rider_priya')), findsOneWidget);
    });

    testWidgets('tapping the Developers card opens the developer portal', (WidgetTester tester) async {
      tester.view.physicalSize = const Size(1400, 1800);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      await tester.pumpWidget(_app());
      await tester.tap(find.byKey(const Key('role-card-Developers')));
      await tester.pumpAndSettle();
      expect(find.text('HexaGo Public API'), findsOneWidget);
      expect(find.byKey(const Key('create-key')), findsOneWidget);
    });

    testWidgets('the download link is web-only by default', (WidgetTester tester) async {
      await tester.pumpWidget(_app());
      expect(find.byKey(const Key('download-apps')), findsNothing); // tests run on the VM, not the web
    });

    testWidgets('tapping on Driver role card opens the driver picker', (WidgetTester tester) async {
      await tester.pumpWidget(_app());
      await tester.tap(find.byKey(const Key('role-card-Driver')));
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 500));
      expect(find.byKey(const ValueKey('driver-search')), findsOneWidget);
      expect(find.byKey(const ValueKey('driver-row-d_001')), findsOneWidget);
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
