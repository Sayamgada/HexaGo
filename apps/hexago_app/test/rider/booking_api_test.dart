import 'dart:convert';
import 'dart:typed_data';

import 'package:dio/dio.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/ops/state/ops_providers.dart';
import 'package:hexago_app/features/rider/data/booking_api.dart';
import 'package:hexago_app/features/rider/models/booking_models.dart';
import 'package:hexago_app/features/rider/widgets/scheduled_legs.dart';

import 'rider_fakes.dart';

/// Answers Dio requests from a handler and records them.
class _Adapter implements HttpClientAdapter {
  _Adapter(this.handler);
  final Object? Function(RequestOptions o) handler;
  final seen = <RequestOptions>[];

  @override
  Future<ResponseBody> fetch(RequestOptions o, Stream<Uint8List>? body, Future<void>? cancel) async {
    seen.add(o);
    return ResponseBody.fromString(jsonEncode(handler(o)), 200, headers: {
      Headers.contentTypeHeader: ['application/json'],
    });
  }

  @override
  void close({bool force = false}) {}
}

void main() {
  test('DioBookingApi talks to the gateway', () async {
    final adapter = _Adapter((o) => switch (o.path) {
          final p when p.endsWith('/personas') => {
              'personas': [
                {'id': 'rider_priya', 'name': 'Priya Sharma', 'kids': ['Anaya'],
                  'saved_places': [{'key': 'home', 'label': 'Home', 'lat': 12.9, 'lng': 77.6}]}
              ]
            },
          final p when p.endsWith('/parse') => goldenPlanJson(),
          final p when p.endsWith('/confirm') => {'plan_id': 'pln_golden', 'scheduled_ride_ids': ['sch_1'], 'demo_mode': true},
          _ => {
              'legs': [
                {'id': 'sch_1', 'plan_id': 'pln_golden', 'seq': 1, 'status': 'dispatched', 'ride_id': 'ride-1',
                  'ride_status': 'matched', 'pickup': {'lat': 1, 'lng': 2}, 'dropoff': {'lat': 3, 'lng': 4},
                  'pickup_at': '2026-11-18T15:15:00+05:30'}
              ]
            },
        });
    final dio = Dio()..httpClientAdapter = adapter;
    final api = DioBookingApi(apiBaseUrl: 'http://gw/', wsBaseUrl: 'ws://gw/', dio: dio);

    final personas = await api.personas();
    expect(personas.single.firstName, 'Priya');
    expect(personas.single.places.single.key, 'home');

    final reply = await api.parse('rider_priya', 'hi', replyTo: 'q_1', forceFallback: true);
    expect(reply, isA<BookingPlan>());
    expect(adapter.seen[1].uri.toString(), 'http://gw/v1/agents/booking/parse?force_fallback=true');
    expect(adapter.seen[1].data, {'rider_id': 'rider_priya', 'text': 'hi', 'reply_to': 'q_1'});
    await api.parse('rider_priya', 'hi');
    expect(adapter.seen[2].data, {'rider_id': 'rider_priya', 'text': 'hi'});

    final res = await api.confirm('pln_golden');
    expect((res.planId, res.demoMode), ('pln_golden', true));
    expect(res.scheduledIds, ['sch_1']);

    final legs = await api.scheduled('rider_priya');
    expect(legs.single.live, isTrue);
    expect(legs.single.pickupAt!.toUtc().hour, 9);
    expect(adapter.seen.last.uri.queryParameters['rider_id'], 'rider_priya');
  });

  test('models parse plans, questions and ride updates', () {
    final plan = BookingReply.fromJson(goldenPlanJson()) as BookingPlan;
    expect(plan.legs.map((l) => l.seq), [1, 2]);
    expect(plan.legs.first.origin.short, "Anaya's school");
    expect(plan.legs.first.price.surging, isTrue);
    expect(plan.answeredBy.tag, 'gemini-3.5-flash-lite');
    final q = BookingReply.fromJson({
      'type': 'question', 'question_id': 'q', 'question': 'When?', 'options': ['Now'],
      'answered_by': {'label': 'rule-based', 'used_fallback': true, 'fallback_reason': 'rate'},
    }) as ClarifyingQuestion;
    expect((q.questionId, q.answeredBy.tag), ('q', 'rule-based (rate)'));
    expect(q.options, ['Now']);
    final u = RideUpdate.fromJson({'ride_id': 'r', 'status': 'matched', 'driver': {'lat': 1, 'lng': '2'}, 'eta_sec': 90});
    expect((u.driverLat, u.driverLng, u.etaSec), (1.0, 2.0, 90));
    expect(RideUpdate.fromJson({'ride_id': 'r', 'status': 'x'}).driverLat, isNull);
    expect(const AnsweredBy().tag, 'rule-based');
  });

  test('scheduled leg status labels', () {
    final now = DateTime(2026, 11, 18, 8);
    expect(legStatusLabel(sched(1, 'scheduled', dispatchAt: now.add(const Duration(seconds: 5))), now), 'Dispatching in 5s');
    expect(legStatusLabel(sched(1, 'scheduled', dispatchAt: now), now), 'Dispatching…');
    expect(legStatusLabel(sched(1, 'dispatched', rideId: 'r', rideStatus: 'requested'), now), 'Finding a driver…');
    expect(legStatusLabel(sched(1, 'dispatched', rideId: 'r', rideStatus: 'in_progress'), now), 'On the trip');
    expect(legStatusLabel(sched(1, 'dispatched', rideId: 'r', rideStatus: 'x'), now), 'Dispatched');
    expect(legStatusLabel(sched(1, 'cancelled', note: 'no-show'), now), 'Cancelled: no-show');
    expect(legStatusLabel(sched(1, 'failed'), now), 'Could not dispatch');
    expect(sched(1, 'failed').finished, isTrue);
  });

  test('mock API offers the golden plan and a clarifying question offline', () async {
    final api = MockBookingApi(now: () => DateTime(2026, 11, 18, 8));
    expect((await api.personas()).length, 3);
    final q = await api.parse('rider_priya', 'hello');
    expect(q, isA<ClarifyingQuestion>());
    final plan = await api.parse('rider_priya', goldenSentence) as BookingPlan;
    expect(plan.legs.length, 2);
    final res = await api.confirm(plan.planId);
    expect(res.scheduledIds.length, 2);
    expect((await api.scheduled('rider_priya')).length, 2);
    expect(await api.rideUpdates('r').isEmpty, isTrue);
  });

  testWidgets('live ride map follows ride:<id> updates', (tester) async {
    final api = FakeBookingApi();
    final leg = sched(1, 'dispatched', rideId: 'ride-1', rideStatus: 'matched');
    await tester.pumpWidget(ProviderScope(
      overrides: [showMapTilesProvider.overrideWithValue(false)],
      child: MaterialApp(home: Scaffold(body: LiveRideMap(leg: leg, updates: api.rideUpdates('ride-1')))),
    ));
    await tester.pump();
    expect(find.text('Live: ride ride-1'), findsOneWidget);
    expect(find.byKey(const Key('live-driver')), findsNothing);
    api.rideStreams['ride-1']!.add(const RideUpdate(rideId: 'ride-1', status: 'in_progress', driverLat: 12.95,
        driverLng: 77.61, etaSec: 300));
    await tester.pump();
    await tester.pump();
    expect(find.text('Live: in progress · ETA 5 min'), findsOneWidget);
    expect(find.byKey(const Key('live-driver')), findsOneWidget);
  });
}
