import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/driver/data/driver_api.dart';
import 'package:hexago_app/features/driver/data/takeover_api.dart';
import 'package:hexago_app/features/driver/driver_home_screen.dart';
import 'package:hexago_app/features/driver/driver_onboarding_screen.dart';
import 'package:hexago_app/features/driver/driver_picker_screen.dart';
import 'package:hexago_app/features/driver/models/driver_models.dart';
import 'package:hexago_app/features/driver/models/takeover_models.dart';
import 'package:hexago_app/features/driver/state/driver_providers.dart';
import 'package:hexago_app/features/driver/widgets/forecast_widgets.dart';
import 'package:hexago_app/features/driver/widgets/ride_offer_card.dart';
import 'package:hexago_app/features/ops/models/ops_models.dart';
import 'package:hexago_app/theme/hexago_theme.dart';

import 'takeover_fakes.dart';

class _OneDriverApi implements DriverApi {
  @override
  Future<List<DriverSummary>> drivers({String? tier, String? query}) async =>
      const [DriverSummary(id: 'd_002', tier: 'mini', status: 'idle')];

  @override
  Future<DriverDetail> driver(String id) async => DriverDetail.fromJson(
      {'id': id, 'tier': 'mini', 'status': 'idle', 'lat': 12.9716, 'lng': 77.5946, 'suspended': false});

  @override
  Future<List<DriverPayout>> payouts(String driverId) async => const [];

  @override
  Future<DriverWallet> wallet(String owner) async => DriverWallet(owner: owner, balancePaise: 0);
}

Widget _wrap(TakeoverApi takeover, Widget child) => ProviderScope(
      overrides: [
        driverApiProvider.overrideWithValue(_OneDriverApi()),
        takeoverApiProvider.overrideWithValue(takeover),
        driverPollProvider.overrideWithValue(const Duration(hours: 1)),
        earningsPollProvider.overrideWithValue(const Duration(hours: 1)),
        forecastPollProvider.overrideWithValue(const Duration(hours: 1)),
        takeoverRenewProvider.overrideWithValue(const Duration(hours: 1)),
        driverPositionsProvider.overrideWithValue(const Stream<List<DriverPosition>>.empty()),
      ],
      child: MaterialApp(theme: buildHexaTheme(), home: child),
    );

final _cells = [
  cell('near_low', 12.972, 77.595, score: 0.3, earn: 120),
  cell('far_rich', 12.99, 77.62, score: 0.8, earn: 410, src: 'llm'),
  cell('mid', 12.98, 77.60, score: 0.6, earn: 260),
  cell('tiny', 12.96, 77.58, score: 0.01, earn: 10),
];

void main() {
  group('models', () {
    test('heatmap, offer, take-over state, result and route parse from JSON', () {
      final m = ForecastHeatmap.fromJson({
        'horizon': 15, 'source': 'postgres', 'scenarios': ['rain_storm'],
        'cells': [
          {'h3': 'a', 'boundary': [[1, 2], [3, 4], [5, 6]], 'center': {'lat': 3, 'lng': 4}, 'demand_score': 1.7,
           'expected_requests_per_hr': 30.5, 'expected_earnings_per_hr': 412, 'explanation': 'x', 'explanation_source': 'llm',
           'trend': 'rising'},
          {'h3': 'b', 'boundary': [[1, 1], [3, 3]], 'demand_score': '0.2'},
        ],
      });
      expect(m.horizon, 15);
      expect(m.scenarios, ['rain_storm']);
      expect(m.cells.first.demandScore, 1.0);
      expect(m.cells.first.fromLlm, isTrue);
      expect(m.cells.first.earningsPerHr, 412);
      expect(m.cells[1].centerLat, 2); // centroid when no centre is sent
      expect(m.cells[1].trend, 'steady');

      final o = RideOffer.fromJson({
        'type': 'ride_offer', 'ride_id': 'r', 'driver_id': 'd', 'pickup': {'lat': 1, 'lng': 2}, 'dropoff': {'lat': 3, 'lng': 4},
        'fare_inr': 180, 'trip_km': 4.2, 'pickup_eta_sec': 90,
      });
      expect(o.isOffer, isTrue);
      expect(o.timeoutS, 15);
      expect(o.dropLng, 4);
      final t = TakeoverState.fromJson({'driver_id': 'd', 'human_controlled': true, 'ttl_s': 599, 'offer': {'type': 'ride_offer'}});
      expect(t.humanControlled, isTrue);
      expect(t.offer, isNotNull);
      expect(OfferResult.fromJson({'ride_id': 'r', 'decision': 'declined', 'rematched_to': 'd_9'}).rematchedTo, 'd_9');
      final r = RoutePath.fromJson({'points': [[1, 2], [3, 4], 'bad'], 'source': 'osrm', 'distance_km': 2.5});
      expect(r.points, [(1.0, 2.0), (3.0, 4.0)]);
    });

    test('hotspots rank by earnings, then demand and distance', () {
      final h = topHotspots(_cells, 12.9716, 77.5946);
      expect(h.map((e) => e.cell.h3), ['far_rich', 'mid', 'near_low']);
      expect(h.first.distanceKm, greaterThan(3));
      expect(haversineKm(12.97, 77.59, 12.97, 77.59), 0);
      expect(horizonLabel(0), 'Now');
      expect(horizonLabel(30), '+30');
      expect(offerClosedMessage(const RideOffer(type: 'offer_closed', rideId: 'r', driverId: 'd', reason: 'timeout')),
          contains('timed out'));
      expect(offerClosedMessage(const RideOffer(type: 'offer_closed', rideId: 'r', driverId: 'd', reason: 'cancelled')),
          contains('cancelled'));
    });

    test('demand colours run cool to hot', () {
      expect(demandColor(0), HexaColors.cyan);
      expect(demandColor(0.5), HexaColors.amber);
      expect(demandColor(1.4), HexaColors.bad);
    });

    test('mock API: heatmap per horizon, offers on send, decline re-matches', () async {
      final api = MockTakeoverApi();
      final now = await api.heatmap(0), later = await api.heatmap(30);
      expect(now.cells, hasLength(18));
      expect(later.cells.first.requestsPerHr, greaterThan(now.cells.first.requestsPerHr));
      expect((await api.takeOver('d_1')).humanControlled, isTrue);
      final next = api.offers('d_1').first;
      await api.sendRide('d_1');
      expect((await next).isOffer, isTrue);
      expect((await api.respond('x', accept: false)).rematchedTo, 'd_017');
      expect((await api.respond('x', accept: true)).decision, 'accepted');
      expect((await api.route(1, 2, 3, 4)).points, hasLength(3));
      await api.release('d_1');
    });
  });

  group('ride offer card', () {
    testWidgets('shows fare, ETA and trip, counts down and disables at zero', (tester) async {
      var accepted = 0, declined = 0;
      await tester.pumpWidget(MaterialApp(
        theme: buildHexaTheme(),
        home: Scaffold(
          body: RideOfferCard(
            offer: offer1, driverLat: 12.9716, driverLng: 77.5946,
            onAccept: () => accepted++, onDecline: () => declined++,
          ),
        ),
      ));
      expect(find.text('₹214'), findsOneWidget);
      expect(find.text('3 min'), findsOneWidget);
      expect(find.text('4.6 km'), findsOneWidget);
      expect(find.text('15 s'), findsOneWidget);
      await tester.pump(const Duration(seconds: 5));
      expect(find.text('10 s'), findsOneWidget);
      await tester.tap(find.byKey(const ValueKey('offer-accept')));
      await tester.tap(find.byKey(const ValueKey('offer-decline')));
      expect((accepted, declined), (1, 1));
      await tester.pump(const Duration(seconds: 11));
      expect(find.text('Timed out'), findsOneWidget);
      await tester.tap(find.byKey(const ValueKey('offer-accept')));
      expect(accepted, 1);
    });
  });

  group('driver home in take-over mode', () {
    testWidgets('takes over, shows hotspots, draws the route, switches horizon and releases on close', (tester) async {
      final api = FakeTakeoverApi(cells: _cells);
      await tester.pumpWidget(_wrap(api, const DriverHomeBody(driverId: 'd_002', showLiveMap: false)));
      await tester.pumpAndSettle();
      expect(api.calls, containsAll(['takeover:d_002', 'heatmap:0']));
      expect(find.byKey(const ValueKey('takeover-banner')), findsOneWidget);
      expect(find.byKey(const ValueKey('hotspot-far_rich')), findsOneWidget);
      expect(find.byKey(const ValueKey('hotspot-tiny')), findsNothing); // top 3 only
      expect(find.text('₹410/h'), findsOneWidget);
      expect(find.text('AI explanation'), findsOneWidget);

      await tester.tap(find.byKey(const ValueKey('hotspot-far_rich')));
      await tester.pumpAndSettle();
      expect(api.calls, contains('route'));
      await tester.tap(find.byKey(const ValueKey('hotspot-far_rich'))); // tap again clears it
      await tester.pumpAndSettle();

      await tester.tap(find.byKey(const ValueKey('horizon-15')));
      await tester.pumpAndSettle();
      expect(api.calls, contains('heatmap:15'));
      expect(find.text('Active: rain_storm'), findsOneWidget);

      await tester.pumpWidget(const SizedBox());
      expect(api.calls, contains('release:d_002'));
    });

    testWidgets('send me a ride, then accept the offer', (tester) async {
      final api = FakeTakeoverApi(cells: _cells);
      await tester.pumpWidget(_wrap(api, const DriverHomeBody(driverId: 'd_002', showLiveMap: false)));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('send-ride')));
      await tester.pump();
      expect(api.calls, contains('send:d_002'));
      api.emit(offer1);
      await tester.pump();
      await tester.pump();
      expect(find.byKey(const ValueKey('offer-card')), findsOneWidget);
      await tester.tap(find.byKey(const ValueKey('offer-accept')));
      await tester.pump();
      expect(api.calls, contains('accept:${offer1.rideId}'));
      expect(find.byKey(const ValueKey('offer-card')), findsNothing);
      expect(find.text('Ride accepted: head to the pickup'), findsOneWidget);
      await tester.pump(const Duration(seconds: 3)); // the delayed take-over check
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('decline shows the re-match; a timed-out offer closes with a notice', (tester) async {
      final api = FakeTakeoverApi(cells: _cells);
      await tester.pumpWidget(_wrap(api, const DriverHomeBody(driverId: 'd_002', showLiveMap: false)));
      await tester.pumpAndSettle();
      api.emit(offer1);
      await tester.pump();
      await tester.pump();
      await tester.tap(find.byKey(const ValueKey('offer-decline')));
      await tester.pump();
      expect(api.calls, contains('decline:${offer1.rideId}'));
      expect(find.text('Declined: re-matched to d_017'), findsOneWidget);

      api.emit(offer1);
      await tester.pump();
      await tester.pump();
      api.emit(const RideOffer(type: 'offer_closed', rideId: 'other', driverId: 'd_002', reason: 'timeout'));
      await tester.pump();
      await tester.pump();
      expect(find.byKey(const ValueKey('offer-card')), findsOneWidget); // not this offer
      api.emit(RideOffer(type: 'offer_closed', rideId: offer1.rideId, driverId: 'd_002', reason: 'timeout'));
      await tester.pump();
      await tester.pump();
      expect(find.byKey(const ValueKey('offer-card')), findsNothing);
      expect(find.text('Offer timed out: re-matched to another driver'), findsOneWidget);
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('an offer the stream missed is picked up from the take-over state', (tester) async {
      final api = FakeTakeoverApi(stateOffer: offer1);
      await tester.pumpWidget(_wrap(api, const DriverHomeBody(driverId: 'd_002', showLiveMap: false)));
      await tester.pumpAndSettle();
      expect(find.byKey(const ValueKey('offer-card')), findsOneWidget);
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('without take-over (disabled server side) the screen stays read-only', (tester) async {
      final api = FakeTakeoverApi(failTakeover: true);
      await tester.pumpWidget(_wrap(api, const DriverHomeBody(driverId: 'd_002', showLiveMap: false)));
      await tester.pumpAndSettle();
      expect(find.byKey(const ValueKey('takeover-banner')), findsNothing);
      await tester.pumpWidget(const SizedBox());
      expect(api.calls, isNot(contains('release:d_002')));
    });
  });

  group('become a driver', () {
    testWidgets('the picker links to the onboarding placeholder', (tester) async {
      var opened = 0;
      await tester.pumpWidget(_wrap(
        FakeTakeoverApi(),
        DriverPickerBody(onPick: (_, _) {}, onBecomeDriver: (_) => opened++, refreshEvery: const Duration(hours: 1)),
      ));
      await tester.pump();
      await tester.tap(find.byKey(const ValueKey('become-driver')));
      expect(opened, 1);

      await tester.pumpWidget(MaterialApp(theme: buildHexaTheme(), home: const DriverOnboardingScreen()));
      expect(find.text('Document verification by Compliance Vision Agent – coming soon'), findsOneWidget);
    });
  });
}
