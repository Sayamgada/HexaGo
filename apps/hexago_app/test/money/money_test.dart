import 'package:dio/dio.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/money/data/ledger_api.dart';
import 'package:hexago_app/features/money/models/ledger_models.dart';
import 'package:hexago_app/features/money/money_widgets.dart';
import 'package:hexago_app/features/ops/models/ops_models.dart';
import 'package:hexago_app/features/ops/widgets/agent_feed.dart';
import 'package:hexago_app/theme/hexago_theme.dart';

import '../ops/fakes.dart';

const _ride = '714fcb12-3a4b-4c5d-8e9f-0123456789ab';

const _summaryJson = {
  'trial_balance_paise': 0,
  'balanced': true,
  'payouts_today': {'sent_count': 1234, 'sent_paise': 2312500, 'held_count': 1, 'held_paise': 9258, 'pending_count': 0},
  'platform_revenue_paise': 675432,
  'rides_settled': 1240,
  'store': 'postgres',
};

Map<String, dynamic> _rideJson({String payout = 'sent', bool interrupted = false}) => {
      'ride_id': _ride,
      'breakdown': {
        'fare_paise': 25000, 'promo_paise': 2500, 'tip_paise': 2000, 'rider_paid_paise': 24500,
        'payee_type': 'driver_payable', 'payee_owner': 'd_001', 'payee_paise': 20750,
        'city_levy_paise': 500, 'gst_paise': 273, 'platform_fee_paise': 5477, 'refunded_paise': 1000,
        'payout_status': payout, 'interrupted': interrupted, 'incident_id': interrupted ? 'inc_42' : '',
      },
      'entries': [
        {
          'id': 'je_1', 'kind': 'ride_settlement', 'description': 'sedan ride', 'created_at': '2026-11-18T10:00:00.123456789Z',
          'postings': [
            {'account': 'payment_clearing:gateway', 'account_type': 'payment_clearing', 'owner': 'gateway', 'amount_paise': 24500, 'memo': 'card'},
            {'account': 'promo_wallet:hexago', 'account_type': 'promo_wallet', 'owner': 'hexago', 'amount_paise': 2500, 'memo': 'promo'},
            {'account': 'driver_payable:d_001', 'account_type': 'driver_payable', 'owner': 'd_001', 'amount_paise': -18750, 'memo': 'payee_share'},
            {'account': 'driver_payable:d_001', 'account_type': 'driver_payable', 'owner': 'd_001', 'amount_paise': -2000, 'memo': 'tip'},
            {'account': 'city_levy_payable:bengaluru', 'account_type': 'city_levy_payable', 'owner': 'bengaluru', 'amount_paise': -500, 'memo': 'city_levy'},
            {'account': 'tax_payable:gst', 'account_type': 'tax_payable', 'owner': 'gst', 'amount_paise': -273, 'memo': 'gst'},
            {'account': 'platform_revenue:hexago', 'account_type': 'platform_revenue', 'owner': 'hexago', 'amount_paise': -5477, 'memo': 'platform_fee'},
          ],
        },
      ],
    };

class FakeLedgerApi implements LedgerApi {
  FakeLedgerApi({LedgerSummary? summary, this.rideLedger}) : summaryValue = summary ?? LedgerSummary.fromJson(_summaryJson);

  LedgerSummary summaryValue;
  RideLedger? rideLedger;
  Object? error;
  int summaryCalls = 0;
  final rides = <String>[];

  @override
  Future<LedgerSummary> summary() async {
    summaryCalls++;
    if (error != null) throw error!;
    return summaryValue;
  }

  @override
  Future<RideLedger> ride(String rideId) async {
    rides.add(rideId);
    if (error != null) throw error!;
    return rideLedger ?? RideLedger.fromJson(_rideJson());
  }
}

Widget _host(LedgerApi api, Widget child) => ProviderScope(
      overrides: [ledgerApiProvider.overrideWithValue(api)],
      child: MaterialApp(theme: ThemeData.dark(), home: Scaffold(body: child)),
    );

Color? _color(WidgetTester tester, String key) => tester.widget<Text>(find.byKey(ValueKey(key))).style?.color;

void main() {
  group('models', () {
    test('formatRupees', () {
      expect(formatRupees(0), '₹0.00');
      expect(formatRupees(5), '₹0.05');
      expect(formatRupees(123456789), '₹1,234,567.89');
      expect(formatRupees(-18750), '-₹187.50');
    });

    test('computeSplit matches dispatch-go ComputeSplit', () {
      final s = computeSplit(farePaise: 25000);
      expect([s.payeePaise, s.cityLevyPaise, s.gstPaise, s.platformFeePaise], [18750, 500, 273, 5477]);
      final f = computeSplit(farePaise: 18001, promoPaise: 2500, tipPaise: 1000, fleet: true);
      expect([f.payeePaise, f.cityLevyPaise, f.gstPaise, f.platformFeePaise, f.riderPaidPaise], [15400, 360, 154, 3087, 16501]);
      expect(f.fleet, isTrue);
      expect(computeSplit(farePaise: 4000, promoPaise: 9000).promoPaise, 4000);
    });

    test('parse summary and ride ledger', () {
      final s = LedgerSummary.fromJson(_summaryJson);
      expect([s.balanced, s.payoutsSentCount, s.heldPaise, s.ridesSettled, s.store], [true, 1234, 9258, 1240, 'postgres']);
      expect(LedgerSummary.fromJson({'trial_balance_paise': 7, 'balanced': true}).balanced, isFalse);
      final r = RideLedger.fromJson(_rideJson());
      expect(r.breakdown!.payeePaise, 20750);
      expect(r.entries.single.sumPaise, 0);
      expect(r.entries.single.postings.first.isDebit, isTrue);
      expect(r.entries.single.createdAt, isNotNull);
      final empty = RideLedger.fromJson({'ride_id': 'x', 'entries': [], 'breakdown': null});
      expect(empty.breakdown, isNull);
      expect(RideLedger.fromJson({'entries': 'junk', 'breakdown': {'fare_paise': '12.4'}}).breakdown!.farePaise, 0);
    });

    test('money events form their own feed category', () {
      for (final k in ['ledger_entry', 'payout_sent', 'payout_held', 'payout_released', 'refund', 'settlement_scan']) {
        expect(sampleEvent(kind: k).category, FeedCategory.money, reason: k);
      }
      expect(FeedFilter.money.accepts(sampleEvent(kind: 'payout_held')), isTrue);
      expect(FeedFilter.money.accepts(sampleEvent(kind: 'match')), isFalse);
    });
  });

  group('api', () {
    test('Dio API hits the gateway ledger endpoints', () async {
      final dio = Dio()..httpClientAdapter = _Adapter();
      final api = DioLedgerApi(apiBaseUrl: 'http://gw/', dio: dio);
      expect((await api.summary()).ridesSettled, 1240);
      final r = await api.ride(_ride);
      expect(r.rideId, _ride);
      expect(_Adapter.lastQuery, {'ride_id': _ride});
    });

    test('mock ledger is balanced and splits deterministically', () async {
      var t = DateTime(2026, 11, 18, 10);
      final api = MockLedgerApi(now: () => t);
      final a = await api.summary();
      t = t.add(const Duration(seconds: 30));
      final b = await api.summary();
      expect(a.balanced && b.balanced, isTrue);
      expect(b.ridesSettled, a.ridesSettled + 10);
      final r1 = await api.ride('ride-abc');
      final r2 = await api.ride('ride-abc');
      expect(r1.breakdown!.farePaise, r2.breakdown!.farePaise);
      for (final e in r1.entries) {
        expect(e.sumPaise, 0, reason: e.kind);
      }
      // Cover both the fleet and the independent branch.
      final kinds = <String>{};
      for (var i = 0; i < 40; i++) {
        kinds.add((await api.ride('r$i')).breakdown!.payeeType);
      }
      expect(kinds, {'driver_payable', 'fleet_operator_payable'});
      expect(MockLedgerApi().runtimeType, MockLedgerApi);
    });
  });

  group('Money tile', () {
    testWidgets('shows a green Balanced ₹0.00, payouts, held and revenue', (tester) async {
      final api = FakeLedgerApi();
      await tester.pumpWidget(_host(api, const MoneyTile(pollEvery: Duration(seconds: 5))));
      expect(find.text('Loading ledger…'), findsOneWidget);
      await tester.pump();
      expect(find.text('Balanced ₹0.00'), findsOneWidget);
      expect(_color(tester, 'money-balance-text'), HexaColors.good);
      expect(find.text('₹23,125.00 · 1,234'), findsOneWidget);
      expect(find.text('₹92.58 · 1'), findsOneWidget);
      expect(_color(tester, 'money-held'), HexaColors.warn);
      expect(find.text('₹6,754.32'), findsOneWidget);
      expect(find.text('1,240'), findsOneWidget);

      // Live: it polls.
      api.summaryValue = LedgerSummary.fromJson({..._summaryJson, 'trial_balance_paise': 5, 'balanced': false});
      await tester.pump(const Duration(seconds: 5));
      await tester.pump();
      expect(api.summaryCalls, 2);
      expect(find.text('Unbalanced ₹0.05'), findsOneWidget);
      expect(_color(tester, 'money-balance-text'), HexaColors.bad);
      await tester.pumpWidget(const SizedBox()); // disposes the timer
    });

    testWidgets('reports an unreachable ledger', (tester) async {
      final api = FakeLedgerApi()..error = Exception('down');
      await tester.pumpWidget(_host(api, const MoneyTile()));
      await tester.pump();
      expect(find.text('Ledger unavailable'), findsOneWidget);
      await tester.pumpWidget(const SizedBox());
    });
  });

  group('fare breakdown', () {
    testWidgets('lists every role and posting', (tester) async {
      final api = FakeLedgerApi();
      await tester.pumpWidget(_host(api, const FareBreakdownView(rideId: _ride)));
      expect(find.byType(CircularProgressIndicator), findsOneWidget);
      await tester.pump();
      for (final (key, text) in [
        ('fare-fare', '₹250.00'),
        ('fare-promo', '₹25.00'),
        ('fare-tip', '₹20.00'),
        ('fare-rider', '₹245.00'),
        ('fare-payee', '₹207.50'),
        ('fare-levy', '₹5.00'),
        ('fare-gst', '₹2.73'),
        ('fare-platform', '₹54.77'),
        ('fare-refunded', '₹10.00'),
      ]) {
        expect(tester.widget<Text>(find.byKey(ValueKey(key))).data, text, reason: key);
      }
      expect(find.text('PAYOUT SENT'), findsOneWidget);
      expect(find.byKey(const ValueKey('entry-je_1')), findsOneWidget);
      expect(find.text('Σ ₹0.00 ✓'), findsOneWidget);
      expect(find.textContaining('driver_payable:d_001'), findsNWidgets(2));
      expect(api.rides, [_ride]);
    });

    testWidgets('held payout and safety escalation are flagged; fleet payee', (tester) async {
      final json = _rideJson(payout: 'held_for_review', interrupted: true);
      (json['breakdown'] as Map)['payee_type'] = 'fleet_operator_payable';
      (json['entries'] as List).add({'id': 'je_bad', 'kind': 'refund', 'postings': [
        {'account': 'rider_wallet:r', 'amount_paise': -5, 'memo': 'refund'},
      ]});
      final api = FakeLedgerApi(rideLedger: RideLedger.fromJson(json));
      await tester.pumpWidget(_host(api, const FareBreakdownView(rideId: _ride)));
      await tester.pump();
      expect(find.text('PAYOUT HELD FOR REVIEW'), findsOneWidget);
      expect(find.text('SAFETY ESCALATION inc_42'), findsOneWidget);
      expect(find.textContaining('Fleet operator', findRichText: true), findsOneWidget);
      expect(find.text('Σ -₹0.05'), findsOneWidget);
    });

    testWidgets('unsettled ride and errors', (tester) async {
      final api = FakeLedgerApi(rideLedger: RideLedger.fromJson({'ride_id': _ride, 'entries': [], 'breakdown': null}));
      await tester.pumpWidget(_host(api, const FareBreakdownView(rideId: _ride)));
      await tester.pump();
      expect(find.byKey(const ValueKey('fare-not-settled')), findsOneWidget);

      final failing = FakeLedgerApi()..error = Exception('boom');
      await tester.pumpWidget(_host(failing, const FareBreakdownView(key: ValueKey('other'), rideId: 'other')));
      await tester.pump();
      expect(find.textContaining('Could not load the ledger'), findsOneWidget);
    });

    testWidgets('opened from a settled ride in the Ops feed', (tester) async {
      tester.view.physicalSize = const Size(1200, 1600);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      final api = FakeLedgerApi();
      final events = [
        sampleEvent(id: 's', agent: 'settlement', kind: 'ledger_entry', rideId: _ride, driverId: 'd_001',
            decision: 'ride 714fcb12 settled: fare ₹250.00'),
        sampleEvent(id: 'm', kind: 'match', rideId: _ride, decision: 'matched ride'),
      ];
      await tester.pumpWidget(_host(api, SizedBox(width: 420, height: 800, child: AgentFeed(events: events))));
      await tester.tap(find.byKey(const ValueKey('feed-filter-Money')));
      await tester.pump();
      expect(find.byType(AgentEventTile), findsOneWidget);
      await tester.tap(find.byType(AgentEventTile));
      await tester.pump(const Duration(milliseconds: 300));
      await tester.pump(const Duration(milliseconds: 300));
      await tester.tap(find.byKey(const ValueKey('fare-breakdown-button')));
      await tester.pump(const Duration(seconds: 1));
      await tester.pump(const Duration(milliseconds: 100));
      expect(find.text('Fare breakdown'), findsWidgets);
      expect(tester.widget<Text>(find.byKey(const ValueKey('fare-platform'))).data, '₹54.77');
      await tester.tap(find.byKey(const ValueKey('fare-close')));
      await tester.pump(const Duration(seconds: 1));
      await tester.pump(const Duration(milliseconds: 100));
      expect(find.byKey(const ValueKey('fare-platform')), findsNothing);
    });

    testWidgets('match events have no fare breakdown button', (tester) async {
      await tester.pumpWidget(_host(FakeLedgerApi(), SizedBox(width: 420, height: 800, child: AgentFeed(events: [
        sampleEvent(id: 'm', kind: 'match', rideId: _ride, decision: 'matched ride'),
      ]))));
      await tester.tap(find.byType(AgentEventTile));
      await tester.pump(const Duration(milliseconds: 300));
      expect(find.byKey(const ValueKey('fare-breakdown-button')), findsNothing);
    });
  });
}

class _Adapter implements HttpClientAdapter {
  static Map<String, dynamic>? lastQuery;

  @override
  Future<ResponseBody> fetch(RequestOptions o, Stream<List<int>>? body, Future<void>? cancel) async {
    lastQuery = o.uri.queryParameters;
    final p = o.uri.path;
    final json = p.endsWith('/v1/ledger/summary')
        ? '{"trial_balance_paise":0,"balanced":true,"rides_settled":1240}'
        : '{"ride_id":"$_ride","entries":[],"breakdown":null}';
    return ResponseBody.fromString(json, 200, headers: {
      Headers.contentTypeHeader: ['application/json'],
    });
  }

  @override
  void close({bool force = false}) {}
}
