import 'package:dio/dio.dart';
import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/ops/models/ops_models.dart';
import 'package:hexago_app/features/ops/ops_command_center_screen.dart';
import 'package:hexago_app/features/ops/state/ops_providers.dart';
import 'package:hexago_app/features/ops/widgets/agent_feed.dart';
import 'package:hexago_app/features/ops/widgets/agents_grid.dart';
import 'package:hexago_app/features/ops/widgets/ops_banner.dart';
import 'package:hexago_app/features/platform/data/platform_api.dart';
import 'package:hexago_app/features/platform/models/platform_models.dart';
import 'package:hexago_app/features/platform/state/platform_providers.dart';
import 'package:hexago_app/features/platform/widgets/surge_layer.dart';
import 'package:hexago_app/util/errors.dart';
import 'package:latlong2/latlong.dart' show LatLng;

import '../ops/fakes.dart';

/// Scriptable PlatformApi: counts calls, can fail.
class FakePlatformApi extends MockPlatformApi {
  FakePlatformApi({this.history = const [], this.failAgents = false});

  final List<AgentEvent> history;
  bool failAgents;
  int agentCalls = 0;
  int surgeCalls = 0;

  @override
  Future<List<AgentInfo>> agents() async {
    agentCalls++;
    if (failAgents) {
      throw DioException(requestOptions: RequestOptions(path: '/v1/agents'), type: DioExceptionType.connectionError);
    }
    return super.agents();
  }

  @override
  Future<SurgeMapData> surgeMap() async {
    surgeCalls++;
    return super.surgeMap();
  }

  @override
  Future<List<AgentEvent>> recentEvents({int limit = 80, int perAgent = 8}) async => history;
}

Widget _host(Widget child, FakePlatformApi api, {List overrides = const []}) => ProviderScope(
  overrides: [platformApiProvider.overrideWithValue(api), ...overrides.cast()],
  child: MaterialApp(home: Scaffold(body: child)),
);

void main() {
  group('models', () {
    test('AgentInfo parses the roster, with defaults for missing fields', () {
      final list = AgentInfo.listFromJson({
        'agents': [
          {
            'id': 'settlement',
            'name': 'Settlement',
            'status': 'active',
            'decisions_last_hour': 61,
            'fallback_rate': 0.25,
            'last_model': 'gemini/gemini-3.5-flash-lite',
            'runs_in': 'agents-py',
            'last_decision': {'decision': 'ledger scan: no anomalies', 'ts': '2026-11-19T10:00:00Z'},
          },
          {'id': 'pool_batching'},
          'junk',
        ],
      });
      expect(list, hasLength(2));
      expect(list[0].active, isTrue);
      expect(list[0].decisionsLastHour, 61);
      expect(list[0].fallbackRate, 0.25);
      expect(list[0].lastDecision, 'ledger scan: no anomalies');
      expect(list[0].lastAt, DateTime.utc(2026, 11, 19, 10));
      expect(list[1].name, 'pool_batching');
      expect(list[1].status, 'planned');
      expect(list[1].lastModel, '');
      expect(AgentInfo.listFromJson('nope'), isEmpty);
    });

    test('SurgeMapData parses cells; only > 1.0x with a polygon are drawn', () {
      final m = SurgeMapData.fromJson({
        'as_of': '2026-11-19T10:00:00Z',
        'source': 'dispatch-go',
        'cells': [
          {
            'cell': 'a',
            'multiplier': 1.4,
            'center': {'lat': 12.9, 'lng': 77.5},
            'boundary': [
              [12.9, 77.5],
              [12.91, 77.5],
              [12.91, 77.51],
              'bad',
            ],
          },
          {
            'cell': 'b',
            'multiplier': 2.5,
            'boundary': [
              [1, 2],
              [3, 4],
              [5, 6],
            ],
            'explanation': 'concert',
          },
          {
            'cell': 'c',
            'multiplier': 1.0,
            'boundary': [
              [1, 2],
              [3, 4],
              [5, 6],
            ],
          },
          {'cell': 'd', 'multiplier': '1.8', 'boundary': []},
        ],
      });
      expect(m.cells, hasLength(4));
      expect(m.cells[0].boundary, hasLength(3));
      expect(m.cells[0].label, '1.4x');
      expect(m.surging.map((c) => c.cell), ['b', 'a']);
      expect(m.surging.first.explanation, 'concert');
      expect(m.asOf, DateTime.utc(2026, 11, 19, 10));
    });

    test('DispatchMetrics reads the persistent counters when present', () {
      final m = DispatchMetrics.fromJson({
        'matches_total': 3,
        'matches_today': 40,
        'rides_settled_today': 31,
        'decisions_today': 900,
      });
      expect((m.matchesToday, m.ridesSettledToday, m.decisionsToday), (40, 31, 900));
      expect(m.toJson()['decisions_today'], 900);
      final old = DispatchMetrics.fromJson({'matches_total': 3});
      expect(old.decisionsToday, isNull);
      expect(old.toJson().containsKey('matches_today'), isFalse);
    });

    test('hexAround makes a closed-ish hexagon around the centre', () {
      final h = hexAround(12.97, 77.59, 0.46);
      expect(h, hasLength(6));
      for (final (lat, lng) in h) {
        expect((lat - 12.97).abs() < 0.005 && (lng - 77.59).abs() < 0.005, isTrue);
      }
    });
  });

  group('errors', () {
    DioException dio({int? code, Object? data, DioExceptionType type = DioExceptionType.badResponse}) => DioException(
      requestOptions: RequestOptions(path: '/x'),
      type: type,
      response: code == null
          ? null
          : Response(
              requestOptions: RequestOptions(path: '/x'),
              statusCode: code,
              data: data,
            ),
      message: 'DioException [bad response]: secret internals',
    );

    test('wake-up errors', () {
      expect(isWakeError(dio(type: DioExceptionType.connectionError)), isTrue);
      expect(isWakeError(dio(type: DioExceptionType.receiveTimeout)), isTrue);
      expect(isWakeError(dio(type: DioExceptionType.unknown)), isTrue);
      expect(isWakeError(dio(code: 502)), isTrue);
      expect(isWakeError(dio(code: 503, data: {'error': 'agents_waking'})), isTrue);
      expect(isWakeError(dio(code: 200, data: {'error': 'agents_waking'})), isTrue);
      expect(isWakeError(dio(code: 404)), isFalse);
      expect(isWakeError(dio(type: DioExceptionType.cancel)), isFalse);
      expect(isWakeError(StateError('x')), isFalse);
    });

    test('friendly messages never leak the raw exception', () {
      final cases = [
        dio(code: 503),
        dio(code: 422, data: {'error': 'unknown tier'}),
        dio(code: 500, data: 'stack trace'),
        dio(type: DioExceptionType.cancel),
        dio(type: DioExceptionType.badCertificate),
        Exception('boom internals'),
      ];
      final texts = [for (final e in cases) friendlyError(e, action: 'plan the trip')];
      expect(texts[0], contains('waking up'));
      expect(texts[1], 'Could not plan the trip: unknown tier');
      expect(texts[2], 'Could not plan the trip (HTTP 500). Please try again.');
      expect(texts[3], 'Request cancelled.');
      expect(texts[4], contains('check your connection'));
      expect(texts[5], 'Could not plan the trip. Please try again.');
      for (final t in texts) {
        expect(t, isNot(contains('DioException')));
        expect(t, isNot(contains('internals')));
        expect(t, isNot(contains('stack trace')));
      }
    });
  });

  group('Agents tab', () {
    testWidgets('grid of 11 cards from GET /v1/agents, planned ones greyed, refreshed every 10 s', (tester) async {
      tester.view.physicalSize = const Size(900, 1400);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      final api = FakePlatformApi();
      await tester.pumpWidget(_host(const SizedBox(width: 900, height: 1300, child: AgentsGrid()), api));
      await tester.pump();
      expect(find.byKey(const ValueKey('agents-active-count')), findsOneWidget);
      expect(find.text('6 of 11 active'), findsOneWidget);
      for (final (id, _, _) in MockPlatformApi.roster) {
        expect(find.byKey(ValueKey('agent-card-$id')), findsOneWidget, reason: id);
      }
      expect(find.byKey(const ValueKey('agent-planned')), findsNWidgets(5));
      expect(find.byKey(const ValueKey('agent-active')), findsNWidgets(6));
      final planned = find.ancestor(
        of: find.byKey(const ValueKey('agent-card-pool_batching')),
        matching: find.byType(Opacity),
      );
      expect(tester.widget<Opacity>(planned).opacity, lessThan(1));
      final settlement = find.byKey(const ValueKey('agent-card-settlement'));
      expect(find.descendant(of: settlement, matching: find.text('gemini-3.5-flash-lite')), findsOneWidget);
      expect(find.descendant(of: settlement, matching: find.text('demo decision by Settlement')), findsOneWidget);
      final broker = find.byKey(const ValueKey('agent-card-dispatch_broker'));
      expect(find.descendant(of: broker, matching: find.text('Deterministic rules')), findsOneWidget);

      expect(api.agentCalls, 1);
      await tester.pump(const Duration(seconds: 10));
      expect(api.agentCalls, 2);
      await tester.pump(const Duration(seconds: 10));
      expect(api.agentCalls, 3);
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('a sleeping agents-py shows a friendly message, then the grid', (tester) async {
      final api = FakePlatformApi(failAgents: true);
      await tester.pumpWidget(_host(const SizedBox(width: 500, height: 800, child: AgentsGrid()), api));
      await tester.pump();
      expect(find.text('Waking up the agent swarm...'), findsOneWidget);
      expect(find.textContaining('DioException'), findsNothing);
      api.failAgents = false;
      await tester.pump(const Duration(seconds: 10));
      await tester.pump();
      expect(find.byKey(const ValueKey('agent-card-booking_intent')), findsOneWidget);
      await tester.pumpWidget(const SizedBox());
    });

    test('model labels', () {
      expect(modelLabel(''), 'Deterministic rules');
      expect(modelLabel('gemini/gemini-3.5-flash-lite'), 'gemini-3.5-flash-lite');
      expect(modelLabel('supply_demand_ratio'), 'supply_demand_ratio');
      expect(modelLabel('weird/'), 'weird/');
    });

    testWidgets('the Ops side panel has Agent feed, Safety and Agents tabs', (tester) async {
      tester.view.physicalSize = const Size(1400, 900);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      final source = FakeOpsSource(initialMetrics: sampleMetrics);
      await tester.pumpWidget(
        ProviderScope(
          overrides: [
            opsSourceProvider.overrideWith((ref) {
              source.start();
              return source;
            }),
            showMapTilesProvider.overrideWithValue(false),
            platformApiProvider.overrideWithValue(FakePlatformApi()),
          ],
          child: const MaterialApp(home: OpsCommandCenterScreen()),
        ),
      );
      await tester.pump();
      expect(find.byKey(const ValueKey('tab-agents')), findsOneWidget);
      await tester.tap(find.byKey(const ValueKey('tab-agents')));
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 400));
      expect(find.byKey(const ValueKey('agents-grid')), findsOneWidget);
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 50));
      expect(find.byKey(const ValueKey('agent-card-booking_intent')), findsOneWidget);
      await tester.pumpWidget(const SizedBox());
    });
  });

  group('feed', () {
    testWidgets('seeds history from every agent below live events and filters per agent', (tester) async {
      final t0 = DateTime.utc(2026, 11, 19, 9);
      final api = FakePlatformApi(
        history: [
          sampleEvent(id: 'h1', agent: 'settlement', decision: 'ledger scan: no anomalies', ts: t0),
          sampleEvent(
            id: 'h2',
            agent: 'ops_supervisor',
            decision: 'heartbeat: swarm healthy',
            ts: t0.subtract(const Duration(minutes: 1)),
          ),
          sampleEvent(
            id: 'h3',
            agent: 'surge_equilibrium',
            decision: 'surge 2.5x in cell 88...',
            ts: t0.subtract(const Duration(minutes: 2)),
          ),
        ],
      );
      final source = FakeOpsSource(initialMetrics: sampleMetrics);
      late WidgetRef seen;
      await tester.pumpWidget(
        ProviderScope(
          overrides: [
            opsSourceProvider.overrideWith((ref) {
              source.start();
              return source;
            }),
            platformApiProvider.overrideWithValue(api),
          ],
          child: MaterialApp(
            home: Scaffold(
              body: Consumer(
                builder: (context, ref, _) {
                  seen = ref;
                  final feed = ref.watch(agentFeedProvider);
                  return SizedBox(
                    width: 600,
                    height: 700,
                    child: AgentFeed(events: feed.events, totalSeen: feed.totalSeen),
                  );
                },
              ),
            ),
          ),
        ),
      );
      source.eventsCtrl.add(sampleEvent(id: 'live', decision: 'Matched live'));
      await tester.pump(const Duration(milliseconds: 300));
      final events = seen.read(agentFeedProvider).events;
      expect(events.map((e) => e.id), ['live', 'h1', 'h2', 'h3']);

      // One chip per roster agent (a horizontally scrolling row); picking one shows only its events.
      final chips = find.byKey(const ValueKey('feed-agent-chips'));
      for (final (id, _) in kRosterAgents) {
        await tester.scrollUntilVisible(
          find.byKey(ValueKey('feed-agent-$id')),
          80,
          scrollable: find.descendant(of: chips, matching: find.byType(Scrollable)),
        );
        expect(find.byKey(ValueKey('feed-agent-$id')), findsOneWidget, reason: id);
      }
      Future<void> showChip(String id) async {
        await tester.drag(chips, const Offset(2000, 0));
        await tester.pump();
        await tester.scrollUntilVisible(
          find.byKey(ValueKey('feed-agent-$id')),
          80,
          scrollable: find.descendant(of: chips, matching: find.byType(Scrollable)),
        );
        await tester.ensureVisible(find.byKey(ValueKey('feed-agent-$id')));
        await tester.pump();
      }

      expect(find.text('Matched live'), findsOneWidget);
      await showChip('settlement');
      await tester.tap(find.byKey(const ValueKey('feed-agent-settlement')));
      await tester.pump();
      expect(find.text('ledger scan: no anomalies'), findsOneWidget);
      expect(find.text('Matched live'), findsNothing);
      await showChip('compliance_vision');
      await tester.tap(find.byKey(const ValueKey('feed-agent-compliance_vision')));
      await tester.pump();
      expect(find.text('No decisions from Compliance Vision yet'), findsOneWidget);
      await tester.tap(find.byKey(const ValueKey('feed-agent-compliance_vision')));
      await tester.pump();
      expect(find.text('Matched live'), findsOneWidget);
      await tester.pumpWidget(const SizedBox());
    });
  });

  group('header', () {
    testWidgets('the banner shows the HexaGo logo and the persistent decision count', (tester) async {
      await tester.pumpWidget(
        const MaterialApp(
          home: Scaffold(
            body: ZeroHumansBanner(decisionsToday: 4321, status: OpsConnectionStatus.live, modeLabel: 'LIVE'),
          ),
        ),
      );
      expect(find.byKey(const ValueKey('hexago-logo')), findsOneWidget);
      expect(find.text('0'), findsNothing);
      expect(find.text('0 humans in the loop · 4,321 autonomous decisions today'), findsOneWidget);
    });

    testWidgets('decisions today come from the server counter when it sends one', (tester) async {
      final source = FakeOpsSource(
        initialMetrics: const DispatchMetrics(
          p50Ms: 1,
          p95Ms: 1,
          p99Ms: 1,
          matchesTotal: 5,
          activeDrivers: 1,
          idleDrivers: 1,
          openRequests: 0,
          ridesInProgress: 0,
          matchesToday: 70,
          ridesSettledToday: 60,
          decisionsToday: 999,
        ),
      );
      late WidgetRef seen;
      await tester.pumpWidget(
        ProviderScope(
          overrides: [
            opsSourceProvider.overrideWith((ref) {
              source.start();
              return source;
            }),
            platformApiProvider.overrideWithValue(FakePlatformApi()),
          ],
          child: MaterialApp(
            home: Consumer(
              builder: (context, ref, _) {
                seen = ref;
                return Text('${ref.watch(decisionsTodayProvider)}');
              },
            ),
          ),
        ),
      );
      source.eventsCtrl.add(sampleEvent(id: 'x'));
      await tester.pump(const Duration(milliseconds: 300));
      expect(seen.read(decisionsTodayProvider), 999, reason: 'not 999 + session events');
      await tester.pumpWidget(const SizedBox());
    });
  });

  group('surge zones', () {
    Widget map(FakePlatformApi api) => _host(
      SizedBox(
        width: 600,
        height: 500,
        child: Stack(
          children: [
            FlutterMap(
              options: const MapOptions(initialCenter: LatLng(12.99, 77.594), initialZoom: 13),
              children: const [SurgeZonesLayer()],
            ),
            const Positioned(left: 8, bottom: 8, child: SurgeToggle()),
          ],
        ),
      ),
      api,
    );

    testWidgets('cells above 1.0x are drawn with labels; the legend toggle hides them', (tester) async {
      final api = FakePlatformApi();
      await tester.pumpWidget(map(api));
      await tester.pump();
      await tester.pump();
      expect(find.byKey(const ValueKey('surge-zones')), findsOneWidget);
      expect(find.text('2.4x'), findsOneWidget);
      expect(find.text('1.6x'), findsOneWidget);
      expect(find.text('Surge zones (3)'), findsOneWidget);

      await tester.tap(find.byKey(const ValueKey('surge-toggle')));
      await tester.pump();
      expect(find.byKey(const ValueKey('surge-zones')), findsNothing);
      expect(find.text('2.4x'), findsNothing);
      await tester.tap(find.byKey(const ValueKey('surge-toggle')));
      await tester.pump();
      expect(find.text('2.4x'), findsOneWidget);

      expect(api.surgeCalls, 1);
      await tester.pump(const Duration(seconds: 15));
      expect(api.surgeCalls, 2);
      await tester.pumpWidget(const SizedBox());
    });

    test('colour runs from amber to red', () {
      expect(surgeColor(1.0), isNot(surgeColor(2.5)));
      expect(surgeColor(3.0), surgeColor(2.5));
    });
  });
}
