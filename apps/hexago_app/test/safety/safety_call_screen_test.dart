import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/safety/data/safety_api.dart';
import 'package:hexago_app/features/safety/safety_call_screen.dart';
import 'package:hexago_app/features/safety/state/safety_providers.dart';

import 'safety_fakes.dart';

class _Clock {
  DateTime t = DateTime.utc(2026, 11, 18, 6, 30);
  DateTime call() => t;
}

Future<_Clock> _pump(WidgetTester tester, FakeSafetyApi api, FakeVoice voice, {Size size = const Size(400, 820)}) async {
  tester.view.physicalSize = size;
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.reset);
  final clock = _Clock();
  await tester.pumpWidget(ProviderScope(
    overrides: [
      safetyApiProvider.overrideWithValue(api),
      voiceProvider.overrideWith((ref) => voice),
    ],
    child: MaterialApp(home: SafetyCallScreen(checkinId: 'chk_1', now: clock.call)),
  ));
  await tester.pump();
  await tester.pump();
  return clock;
}

Future<void> _advance(WidgetTester tester, _Clock clock, Duration d) async {
  clock.t = clock.t.add(d);
  await tester.pump(const Duration(milliseconds: 250));
}

void main() {
  testWidgets('rings with the spoken question and a visible countdown', (tester) async {
    final api = FakeSafetyApi();
    final voice = FakeVoice();
    final clock = await _pump(tester, api, voice);

    expect(find.text('HexaGo Safety'), findsOneWidget);
    expect(find.text('Incoming safety call'), findsOneWidget);
    expect(find.text(sampleQuestion), findsOneWidget);
    expect(voice.spoken, [sampleQuestion]);
    expect(find.byKey(const ValueKey('safety-countdown')), findsOneWidget);
    expect(find.text('15'), findsOneWidget);
    // Typed reply is always available; no mic without speech recognition.
    expect(find.byKey(const ValueKey('safety-reply-field')), findsOneWidget);
    expect(find.byKey(const ValueKey('safety-mic')), findsNothing);

    await _advance(tester, clock, const Duration(seconds: 5));
    expect(find.text('10'), findsOneWidget);
    expect(find.textContaining('Answer within 10 s of 15 s'), findsOneWidget);

    await tester.tap(find.byKey(const ValueKey('safety-replay-question')));
    expect(voice.spoken.length, 2);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('typed "I\'m fine" sends the transcript and shows the monitor outcome', (tester) async {
    final api = FakeSafetyApi();
    await _pump(tester, api, FakeVoice());
    await tester.enterText(find.byKey(const ValueKey('safety-reply-field')), "I'm fine");
    await tester.tap(find.byKey(const ValueKey('safety-send')));
    await tester.pump();
    await tester.pump();
    expect(api.replies, ["I'm fine"]);
    expect(find.byKey(const ValueKey('safety-call-safe')), findsOneWidget);
    expect(find.textContaining("glad you're safe"), findsOneWidget);
    expect(find.text('Call ended'), findsOneWidget);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('"I need help" escalates', (tester) async {
    final api = FakeSafetyApi();
    final voice = FakeVoice();
    await _pump(tester, api, voice);
    await tester.tap(find.byKey(const ValueKey('quick-help')));
    await tester.pump();
    await tester.pump();
    expect(api.replies, ['I need help']);
    expect(find.byKey(const ValueKey('safety-call-help')), findsOneWidget);
    expect(find.textContaining('Help is on the way'), findsOneWidget);
    expect(voice.spoken.last, contains('Help is on the way'));
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('quick "I\'m fine" and empty text', (tester) async {
    final api = FakeSafetyApi();
    await _pump(tester, api, FakeVoice());
    await tester.tap(find.byKey(const ValueKey('safety-send'))); // empty: ignored
    await tester.pump();
    expect(api.replies, isEmpty);
    await tester.tap(find.byKey(const ValueKey('quick-fine')));
    await tester.pump();
    await tester.pump();
    expect(api.replies, ["I'm fine"]);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('no answer before the countdown ends shows escalation, then the escalated status', (tester) async {
    final api = FakeSafetyApi();
    final clock = await _pump(tester, api, FakeVoice());
    await _advance(tester, clock, const Duration(seconds: 15));
    expect(find.byKey(const ValueKey('safety-call-timeout')), findsOneWidget);
    expect(find.textContaining('No answer'), findsOneWidget);
    expect(find.byKey(const ValueKey('safety-reply-field')), findsNothing);

    api.call = sampleCall(status: 'timed_out', remaining: 0, incidentStatus: 'escalated');
    await tester.pump(const Duration(seconds: 2));
    await tester.pump();
    expect(find.textContaining('Escalated: help is on the way'), findsOneWidget);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('polling gives up after ten tries and survives errors', (tester) async {
    final api = FakeSafetyApi();
    final clock = await _pump(tester, api, FakeVoice());
    await _advance(tester, clock, const Duration(seconds: 16));
    api.loadError = Exception('offline');
    for (var i = 0; i < 12; i++) {
      await tester.pump(const Duration(seconds: 2));
    }
    expect(find.textContaining('No answer'), findsOneWidget);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('answers by voice where speech recognition is available', (tester) async {
    final api = FakeSafetyApi();
    final voice = FakeVoice(speech: true);
    await _pump(tester, api, voice);
    expect(find.byKey(const ValueKey('safety-mic')), findsOneWidget);
    await tester.tap(find.byKey(const ValueKey('safety-mic')));
    await tester.pump();
    expect(voice.listening, isTrue);
    expect(find.text('Listening...'), findsOneWidget);
    voice.onWords!('please', false);
    await tester.pump();
    expect(find.text('please'), findsOneWidget);
    voice.onWords!('please help me', true);
    await tester.pump();
    await tester.pump();
    expect(api.replies, ['please help me']);
    expect(find.byKey(const ValueKey('safety-call-help')), findsOneWidget);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('the mic toggles off again', (tester) async {
    final voice = FakeVoice(speech: true);
    await _pump(tester, FakeSafetyApi(), voice);
    await tester.tap(find.byKey(const ValueKey('safety-mic')));
    await tester.pump();
    await tester.tap(find.byKey(const ValueKey('safety-mic')));
    await tester.pump();
    expect(voice.listening, isFalse);
    expect(find.text('Type your answer'), findsOneWidget);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('a call that already ended shows its outcome', (tester) async {
    final api = FakeSafetyApi(call: sampleCall(status: 'answered', incidentStatus: 'monitoring'));
    await _pump(tester, api, FakeVoice());
    expect(find.byKey(const ValueKey('safety-call-closed')), findsOneWidget);
    expect(find.textContaining('You told us you are safe'), findsOneWidget);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('a reply after the server closed the call shows the outcome', (tester) async {
    final api = FakeSafetyApi()
      ..replyError = CheckinClosedException(sampleCall(status: 'timed_out', incidentStatus: 'escalated'));
    await _pump(tester, api, FakeVoice());
    await tester.tap(find.byKey(const ValueKey('quick-fine')));
    await tester.pump();
    await tester.pump();
    expect(find.textContaining('escalated: help is on the way'), findsOneWidget);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('a failed send lets the rider retry', (tester) async {
    final api = FakeSafetyApi()..replyError = Exception('network');
    await _pump(tester, api, FakeVoice());
    await tester.tap(find.byKey(const ValueKey('quick-fine')));
    await tester.pump();
    await tester.pump();
    expect(find.textContaining('Could not send your answer'), findsOneWidget);
    api.replyError = null;
    await tester.tap(find.byKey(const ValueKey('quick-fine')));
    await tester.pump();
    await tester.pump();
    expect(find.byKey(const ValueKey('safety-call-safe')), findsOneWidget);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('an unreachable service shows an error', (tester) async {
    final api = FakeSafetyApi()..loadError = Exception('down');
    await _pump(tester, api, FakeVoice());
    expect(find.byKey(const ValueKey('safety-call-error')), findsOneWidget);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('opens as a phone-sized dialog from the Ops console', (tester) async {
    final api = FakeSafetyApi();
    await tester.pumpWidget(ProviderScope(
      overrides: [safetyApiProvider.overrideWithValue(api), voiceProvider.overrideWith((ref) => FakeVoice())],
      child: MaterialApp(
        home: Builder(builder: (context) => TextButton(onPressed: () => showRiderPhone(context, 'chk_1'), child: const Text('open'))),
      ),
    ));
    await tester.tap(find.text('open'));
    await tester.pumpAndSettle(const Duration(milliseconds: 100), EnginePhase.sendSemanticsUpdate, const Duration(seconds: 2));
    expect(find.byKey(const ValueKey('rider-phone-dialog')), findsOneWidget);
    expect(find.text(sampleQuestion), findsOneWidget);
    expect(tester.getSize(find.byKey(const ValueKey('safety-call-screen'))).width, lessThanOrEqualTo(380));
    await tester.tap(find.byKey(const ValueKey('safety-call-close')));
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 300));
    expect(find.byKey(const ValueKey('rider-phone-dialog')), findsNothing);
    await tester.pumpWidget(const SizedBox());
  });
}
