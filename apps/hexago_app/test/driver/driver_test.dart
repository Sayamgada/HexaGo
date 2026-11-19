import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/driver/data/driver_api.dart';
import 'package:hexago_app/features/driver/driver_home_screen.dart';
import 'package:hexago_app/features/driver/driver_picker_screen.dart';
import 'package:hexago_app/features/driver/models/driver_models.dart';
import 'package:hexago_app/features/driver/state/driver_providers.dart';
import 'package:hexago_app/features/driver/widgets/driver_earnings.dart';
import 'package:hexago_app/features/ops/models/ops_models.dart';
import 'package:hexago_app/theme/hexago_theme.dart';

final _now = DateTime.utc(2026, 11, 19, 8); // 13:30 IST

class FakeDriverApi implements DriverApi {
  FakeDriverApi({this.list = const [], this.detail, this.payoutList = const [], this.walletValue});

  final List<DriverSummary> list;
  final DriverDetail? detail;
  final List<DriverPayout> payoutList;
  final DriverWallet? walletValue;
  final walletOwners = <String>[];

  @override
  Future<List<DriverSummary>> drivers({String? tier, String? query}) async => list;

  @override
  Future<DriverDetail> driver(String id) async => detail ?? (throw StateError('no detail'));

  @override
  Future<List<DriverPayout>> payouts(String driverId) async => payoutList;

  @override
  Future<DriverWallet> wallet(String owner) async {
    walletOwners.add(owner);
    return walletValue ?? DriverWallet(owner: owner, balancePaise: 0);
  }
}

Widget _wrap(FakeDriverApi api, Widget child) => ProviderScope(
      overrides: [
        driverApiProvider.overrideWithValue(api),
        driverPollProvider.overrideWithValue(const Duration(hours: 1)),
        earningsPollProvider.overrideWithValue(const Duration(hours: 1)),
        driverPositionsProvider.overrideWithValue(const Stream<List<DriverPosition>>.empty()),
      ],
      child: MaterialApp(theme: buildHexaTheme(), home: child),
    );

const _drivers = [
  DriverSummary(id: 'd_001', tier: 'sedan', status: 'on_trip', rating: 4.8, rideId: 'r1'),
  DriverSummary(id: 'd_002', tier: 'av', status: 'idle', av: true, batteryPct: 81),
  DriverSummary(id: 'd_042', tier: 'mini', status: 'en_route'),
  DriverSummary(id: 'd_007', tier: 'auto', status: 'suspended', suspended: true, incidentId: 'inc_9'),
];

void main() {
  group('driver picker', () {
    testWidgets('lists drivers with status, AV badge, search and tier filter', (tester) async {
      final picked = <String>[];
      await tester.pumpWidget(_wrap(
        FakeDriverApi(list: _drivers),
        DriverPickerBody(onPick: (_, d) => picked.add(d.id), refreshEvery: const Duration(hours: 1)),
      ));
      await tester.pump();

      for (final d in _drivers) {
        expect(find.byKey(ValueKey('driver-row-${d.id}')), findsOneWidget);
      }
      expect(find.byKey(const ValueKey('av-badge-d_002')), findsOneWidget);
      expect(find.byKey(const ValueKey('av-badge-d_001')), findsNothing);
      expect(find.text('4 of 4 drivers · 1 on a trip'), findsOneWidget);
      expect(
        find.descendant(of: find.byKey(const ValueKey('driver-row-d_007')), matching: find.text('Suspended')),
        findsOneWidget,
      );
      expect(
        find.descendant(of: find.byKey(const ValueKey('driver-row-d_042')), matching: find.text('To pickup')),
        findsOneWidget,
      );

      await tester.enterText(find.byKey(const ValueKey('driver-search')), '042');
      await tester.pump();
      expect(find.byKey(const ValueKey('driver-row-d_042')), findsOneWidget);
      expect(find.byKey(const ValueKey('driver-row-d_001')), findsNothing);

      await tester.enterText(find.byKey(const ValueKey('driver-search')), '');
      await tester.tap(find.byKey(const ValueKey('tier-av')));
      await tester.pump();
      expect(find.byKey(const ValueKey('driver-row-d_002')), findsOneWidget);
      expect(find.byKey(const ValueKey('driver-row-d_001')), findsNothing);

      await tester.tap(find.byKey(const ValueKey('tier-sedan')));
      await tester.pump();
      await tester.tap(find.byKey(const ValueKey('driver-row-d_001')));
      expect(picked, ['d_001']);

      await tester.enterText(find.byKey(const ValueKey('driver-search')), 'zzz');
      await tester.pump();
      expect(find.byKey(const ValueKey('driver-empty')), findsOneWidget);
    });
  });

  group('driver home', () {
    testWidgets('a driver suspended by Safety shows the frozen banner with the incident id', (tester) async {
      final api = FakeDriverApi(
        detail: DriverDetail.fromJson(const {
          'id': 'd_007', 'tier': 'av', 'status': 'suspended', 'av': true, 'battery_pct': 64.0, 'lat': 12.97, 'lng': 77.59,
          'suspended': true, 'incident_id': 'inc_7f3a',
          'suspension': {'incident_id': 'inc_7f3a', 'reason': 'route deviation', 'suspended_at': '2026-11-19T07:55:00Z'},
          'av_status': {'battery_pct': 64.0, 'health': 'ok', 'health_note': 'All systems nominal', 'mode': 'remote_hold'},
        }),
      );
      await tester.pumpWidget(_wrap(api, const DriverHomeBody(driverId: 'd_007', showLiveMap: false)));
      await tester.pump();

      expect(find.byKey(const ValueKey('frozen-banner')), findsOneWidget);
      expect(find.text('Account frozen by Safety Sentinel – under review'), findsOneWidget);
      expect(find.text('Incident inc_7f3a'), findsOneWidget);
      expect(find.byKey(const ValueKey('ride-card-empty')), findsOneWidget);
      expect(find.byKey(const ValueKey('av-panel')), findsOneWidget);
      expect(find.text('Remote hold (Safety)'), findsOneWidget);
      expect(find.text('64%'), findsOneWidget);
    });

    testWidgets('a driver on a trip shows the ride card and no banner', (tester) async {
      final api = FakeDriverApi(
        detail: DriverDetail.fromJson(const {
          'id': 'd_001', 'tier': 'sedan', 'status': 'on_trip', 'lat': 12.97, 'lng': 77.59, 'suspended': false,
          'ride': {
            'id': '714fcb12-3a4b-4c5d-8e9f-0123456789ab', 'rider_id': 'r_042', 'status': 'on_trip',
            'pickup': {'lat': 12.97, 'lng': 77.59}, 'dropoff': {'lat': 12.99, 'lng': 77.61}, 'eta_sec': 410, 'progress': 0.25,
          },
        }),
      );
      await tester.pumpWidget(_wrap(api, const DriverHomeBody(driverId: 'd_001', showLiveMap: false)));
      await tester.pump();

      expect(find.byKey(const ValueKey('frozen-banner')), findsNothing);
      expect(find.byKey(const ValueKey('ride-card')), findsOneWidget);
      expect(find.text('ETA 7 min to drop-off'), findsOneWidget);
      expect(find.text('r_042'), findsOneWidget);
      expect(find.text('25% of the way to drop-off'), findsOneWidget);
      expect(find.byKey(const ValueKey('av-panel')), findsNothing);
      expect(find.text('On trip'), findsOneWidget);
    });
  });

  group('earnings', () {
    DriverPayout po(String ride, int paise, String status, Duration ago, {String incident = '', String owner = 'd_001'}) =>
        DriverPayout(id: 'po_$ride', rideId: ride, owner: owner, amountPaise: paise, status: status, incidentId: incident,
            createdAt: _now.subtract(ago));

    testWidgets("lists today's trips with totals, paid / held payouts and the wallet", (tester) async {
      final api = FakeDriverApi(
        payoutList: [
          po('ride_a', 18750, 'sent', const Duration(minutes: 10)),
          po('ride_b', 20750, 'held_for_review', const Duration(minutes: 40), incident: 'inc_9'),
          po('ride_c', 15000, 'sent', const Duration(hours: 2)),
          po('ride_old', 99900, 'sent', const Duration(hours: 20)), // yesterday (IST)
        ],
        walletValue: const DriverWallet(owner: 'd_001', balancePaise: 20750, paidOutPaise: 33750, heldPaise: 20750),
      );
      await tester.pumpWidget(_wrap(api, Scaffold(body: DriverEarningsView(driverId: 'd_001', now: () => _now))));
      await tester.pump();

      expect(find.byKey(const ValueKey('earnings-trips')), findsOneWidget);
      expect(tester.widget<Text>(find.byKey(const ValueKey('earnings-trips'))).data, '3');
      expect(tester.widget<Text>(find.byKey(const ValueKey('earnings-total'))).data, '₹545.00');
      expect(tester.widget<Text>(find.byKey(const ValueKey('earnings-paid'))).data, '₹337.50');
      expect(tester.widget<Text>(find.byKey(const ValueKey('earnings-held'))).data, '₹207.50');
      expect(tester.widget<Text>(find.byKey(const ValueKey('earnings-wallet'))).data, '₹207.50');
      expect(find.byKey(const ValueKey('trip-ride_a')), findsOneWidget);
      expect(find.byKey(const ValueKey('trip-ride_old')), findsNothing);
      expect(tester.widget<Text>(find.byKey(const ValueKey('trip-status-ride_b'))).data, 'Held for review · incident inc_9');
      expect(tester.widget<Text>(find.byKey(const ValueKey('trip-status-ride_a'))).data, 'Paid');
      expect(api.walletOwners, ['d_001']);
    });

    testWidgets('an AV shows its fleet operator wallet', (tester) async {
      final api = FakeDriverApi(payoutList: [po('ride_x', 16000, 'sent', const Duration(minutes: 5), owner: 'fleet_op_blr')]);
      await tester.pumpWidget(_wrap(api, Scaffold(body: DriverEarningsView(driverId: 'd_002', now: () => _now))));
      await tester.pump();
      expect(api.walletOwners, ['fleet_op_blr']);
      expect(find.text('Wallet (fleet_op_blr)'), findsOneWidget);
    });

    testWidgets('no trips today', (tester) async {
      await tester.pumpWidget(_wrap(FakeDriverApi(), Scaffold(body: DriverEarningsView(driverId: 'd_003', now: () => _now))));
      await tester.pump();
      expect(find.byKey(const ValueKey('earnings-empty')), findsOneWidget);
    });
  });

  test('startOfDayIst and formatEta', () {
    expect(startOfDayIst(DateTime.utc(2026, 11, 19, 8)), DateTime.utc(2026, 11, 18, 18, 30));
    expect(startOfDayIst(DateTime.utc(2026, 11, 19, 20)), DateTime.utc(2026, 11, 19, 18, 30));
    expect(formatEta(0), 'now');
    expect(formatEta(40), '1 min');
    expect(formatEta(3720), '1 h 2 min');
    expect(DriverPhase.fromWire('en_route'), DriverPhase.toPickup);
    expect(DriverPhase.fromWire('idle', suspended: true), DriverPhase.suspended);
  });

  test('mock api: suspended d_007 and moving trips', () async {
    var t = DateTime.utc(2026, 11, 19, 8);
    final api = MockDriverApi(now: () => t);
    final all = await api.drivers();
    expect(all, hasLength(40));
    expect((await api.driver('d_007')).suspended, isTrue);
    final a = await api.driver('d_003');
    t = t.add(const Duration(seconds: 20));
    final b = await api.driver('d_003');
    expect(a.ride, isNotNull);
    expect((a.summary.lat, a.summary.lng), isNot((b.summary.lat, b.summary.lng)));
    expect((await api.drivers(tier: 'av')).every((d) => d.av), isTrue);
  });
}
