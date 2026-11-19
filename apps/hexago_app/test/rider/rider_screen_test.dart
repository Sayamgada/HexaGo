import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/rider/models/booking_models.dart';
import 'package:hexago_app/features/rider/rider_screen.dart';
import 'package:hexago_app/features/rider/state/rider_providers.dart';
import 'package:hexago_app/features/safety/state/safety_providers.dart';
import 'package:hexago_app/features/safety/voice/voice_io.dart';
import 'package:hexago_app/theme/hexago_theme.dart';

import '../safety/safety_fakes.dart' show FakeVoice;
import 'rider_fakes.dart';

final t0 = DateTime(2026, 11, 18, 8);

Future<void> pumpRider(WidgetTester tester, FakeBookingApi api,
    {String? persona = 'rider_priya', VoiceIO? voice}) async {
  tester.view.physicalSize = const Size(1000, 1600);
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.reset);
  await tester.pumpWidget(ProviderScope(
    overrides: [
      bookingApiProvider.overrideWithValue(api),
      voiceProvider.overrideWith((ref) => voice ?? SilentVoiceIO()),
    ],
    child: MaterialApp(
      theme: buildHexaTheme(),
      home: RiderScreen(initialPersonaId: persona, showLiveMap: false, now: () => t0,
          pollEvery: const Duration(seconds: 3)),
    ),
  ));
  await tester.pump();
}

Future<void> typeAndSend(WidgetTester tester, String text) async {
  await tester.enterText(find.byKey(const Key('booking-input')), text);
  await tester.tap(find.byKey(const Key('booking-send')));
  await tester.pump();
}

void main() {
  testWidgets('persona picker then the big plan box', (tester) async {
    final api = FakeBookingApi();
    await pumpRider(tester, api, persona: null);
    expect(find.text('Who is riding today?'), findsOneWidget);
    await tester.tap(find.byKey(const Key('persona-rider_arjun')));
    await tester.pump();
    expect(find.text('Where to? Or describe your whole plan...'), findsOneWidget);
    expect(find.text('Arjun Rao'), findsOneWidget);
    await tester.tap(find.byKey(const Key('persona-chip')));
    await tester.pump();
    expect(find.text('Who is riding today?'), findsOneWidget);
  });

  testWidgets('golden sentence: planning state, timeline, confirm, scheduled legs and dispatch', (tester) async {
    final api = FakeBookingApi()..holdParse = Completer<void>();
    await pumpRider(tester, api);
    await typeAndSend(tester, goldenSentence);

    expect(find.byKey(const Key('planning')), findsOneWidget);
    expect(find.text('Planning your trip...'), findsOneWidget);
    expect(find.text(goldenSentence), findsOneWidget); // the rider's message
    api.holdParse!.complete();
    await tester.pump();
    await tester.pump();

    expect(api.parses.single.rider, 'rider_priya');
    expect(api.parses.single.force, isFalse);
    expect(find.byKey(const Key('planning')), findsNothing);
    expect(find.byKey(const Key('plan-pln_golden')), findsOneWidget);
    expect(find.text('gemini-3.5-flash-lite'), findsOneWidget);

    api.scheduledLegs = [sched(1, 'scheduled', dispatchAt: t0.add(const Duration(seconds: 20))), sched(2, 'scheduled')];
    await tester.tap(find.byKey(const Key('plan-confirm')));
    await tester.pump();
    await tester.pump();
    expect(api.confirms, ['pln_golden']);
    expect(find.textContaining('first leg dispatches in 20 s'), findsOneWidget);
    expect(find.byKey(const Key('scheduled-panel')), findsOneWidget);
    expect(find.text('Dispatching in 20s'), findsOneWidget);
    expect(find.text('Waiting for the previous ride'), findsOneWidget);
    expect(find.byKey(const Key('plan-confirm')), findsNothing);

    // The scheduler dispatches leg 1: the next poll shows it live.
    api.scheduledLegs = [sched(1, 'dispatched', rideId: 'ride-1', rideStatus: 'matched'), sched(2, 'scheduled')];
    await tester.pump(const Duration(seconds: 3));
    await tester.pump();
    expect(find.text('Driver d_042 on the way'), findsOneWidget);

    api.scheduledLegs = [sched(1, 'completed'), sched(2, 'completed')];
    await tester.pump(const Duration(seconds: 3));
    await tester.pump();
    expect(find.text('Completed'), findsNWidgets(2));
    await tester.pumpWidget(const SizedBox()); // stop timers
  });

  testWidgets('clarifying question shows quick-answer chips that reply to it', (tester) async {
    final api = FakeBookingApi(replies: [
      const ClarifyingQuestion(questionId: 'q_1', question: "Which school - Aanya's school or Kabir's school?",
          options: ["Aanya's school", "Kabir's school"]),
    ]);
    await pumpRider(tester, api, persona: 'rider_arjun');
    await typeAndSend(tester, 'Pick up my kid from school at 3:15 PM and drop them at home');
    await tester.pump();
    expect(find.text("Which school - Aanya's school or Kabir's school?"), findsOneWidget);
    expect(find.text('rule-based'), findsOneWidget);
    await tester.tap(find.byKey(const Key("chip-Kabir's school")));
    await tester.pump();
    await tester.pump();
    expect(api.parses.last.text, "Kabir's school");
    expect(api.parses.last.replyTo, 'q_1');
    expect(find.byKey(const Key('plan-pln_golden')), findsOneWidget);
  });

  testWidgets('Edit puts the request back in the box; errors are shown', (tester) async {
    final api = FakeBookingApi();
    await pumpRider(tester, api);
    await typeAndSend(tester, goldenSentence);
    await tester.pump();
    await tester.tap(find.byKey(const Key('plan-edit')));
    await tester.pump();
    expect(find.byKey(const Key('plan-pln_golden')), findsNothing);
    expect(tester.widget<TextField>(find.byKey(const Key('booking-input'))).controller!.text, goldenSentence);

    api.parseError = Exception('gateway asleep');
    await tester.tap(find.byKey(const Key('booking-send')));
    await tester.pump();
    await tester.pump();
    expect(find.textContaining('Could not plan the trip'), findsOneWidget);
    expect(find.textContaining('gateway asleep'), findsNothing, reason: 'never the raw exception');

    api.parseError = null;
    api.confirmError = Exception('scheduler down');
    await typeAndSend(tester, 'from home to the office at 9 am');
    await tester.pump();
    await tester.tap(find.byKey(const Key('plan-confirm')));
    await tester.pump();
    await tester.pump();
    expect(find.textContaining('Could not schedule the rides'), findsOneWidget);
  });

  testWidgets('force rule-based toggle is sent with the request', (tester) async {
    final api = FakeBookingApi();
    await pumpRider(tester, api);
    await tester.tap(find.byKey(const Key('rider-menu')));
    await tester.pumpAndSettle();
    await tester.tap(find.byType(CheckedPopupMenuItem<String>));
    await tester.pumpAndSettle();
    expect(find.text('Rule-based parser forced (demo)'), findsOneWidget);
    await typeAndSend(tester, goldenSentence);
    await tester.pump();
    expect(api.parses.single.force, isTrue);
    expect(find.text('rule-based (forced)'), findsOneWidget);
  });

  testWidgets('voice fills the box with the transcript and waits for send', (tester) async {
    final api = FakeBookingApi();
    final voice = FakeVoice(speech: true);
    await pumpRider(tester, api, voice: voice);
    await tester.pump();
    await tester.tap(find.byKey(const Key('booking-mic')));
    await tester.pump();
    voice.onWords!('take me to the airport', true);
    await tester.pump();
    expect(tester.widget<TextField>(find.byKey(const Key('booking-input'))).controller!.text, 'take me to the airport');
    expect(find.byKey(const Key('voice-transcript-hint')), findsOneWidget);
    expect(api.parses, isEmpty); // nothing is sent until the rider presses send
    await tester.tap(find.byKey(const Key('booking-send')));
    await tester.pump();
    expect(api.parses.single.text, 'take me to the airport');
  });

  testWidgets('personas that fail to load show an error', (tester) async {
    await pumpRider(tester, _BrokenApi(), persona: null);
    expect(find.byKey(const Key('rider-error')), findsOneWidget);
  });
}

class _BrokenApi extends FakeBookingApi {
  @override
  Future<List<Persona>> personas() async => throw Exception('offline');
}
