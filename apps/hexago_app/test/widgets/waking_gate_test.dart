import 'dart:async';

import 'package:dio/dio.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/widgets/waking_gate.dart';

/// Probe double: fails while [down], records whether agents-py was asked for.
class FakeProbe {
  bool down;
  Completer<void>? hold; // a probe that hangs until completed
  int calls = 0;
  final agentsAsked = <bool>[];

  FakeProbe({this.down = false});

  Future<void> call({required bool agents}) async {
    calls++;
    agentsAsked.add(agents);
    if (hold != null) await hold!.future;
    if (down) {
      throw DioException(
        requestOptions: RequestOptions(path: '/healthz'),
        type: DioExceptionType.badResponse,
        response: Response(requestOptions: RequestOptions(path: '/healthz'), statusCode: 503),
        message: 'DioException [bad response]: 503 raw text that must never show',
      );
    }
  }
}

/// A child that counts how often it was (re)built from scratch.
class _Child extends StatefulWidget {
  const _Child({required this.mounts});
  final List<int> mounts;

  @override
  State<_Child> createState() => _ChildState();
}

class _ChildState extends State<_Child> {
  @override
  void initState() {
    super.initState();
    widget.mounts.add(1);
  }

  @override
  Widget build(BuildContext context) => Center(
    child: Column(
      mainAxisSize: MainAxisSize.min,
      children: [
        const Text('the screen', key: ValueKey('child')),
        TextButton(
          key: const ValueKey('fail'),
          onPressed: () => WakingGate.report(
            context,
            DioException(
              requestOptions: RequestOptions(path: '/v1/agents'),
              type: DioExceptionType.connectionError,
            ),
          ),
          child: const Text('call the backend'),
        ),
        TextButton(
          key: const ValueKey('fail-other'),
          onPressed: () => WakingGate.report(context, StateError('not a wake-up error')),
          child: const Text('other error'),
        ),
      ],
    ),
  );
}

Future<List<int>> _pump(WidgetTester tester, FakeProbe probe, {bool needsAgents = false}) async {
  final mounts = <int>[];
  await tester.pumpWidget(
    ProviderScope(
      overrides: [wakeProbeProvider.overrideWithValue(probe.call)],
      child: MaterialApp(
        home: Scaffold(
          body: WakingGate(
            needsAgents: needsAgents,
            child: _Child(mounts: mounts),
          ),
        ),
      ),
    ),
  );
  return mounts;
}

void main() {
  testWidgets('a backend that answers shows the screen right away', (tester) async {
    final probe = FakeProbe();
    final mounts = await _pump(tester, probe);
    expect(find.byKey(const ValueKey('child')), findsOneWidget);
    await tester.pump(const Duration(seconds: 3));
    expect(find.byKey(const ValueKey('waking-screen')), findsNothing);
    expect(mounts, hasLength(1));
    expect(probe.agentsAsked, [false]);
  });

  testWidgets('asleep: friendly waking screen, retries every 5 s, then the screen reloads', (tester) async {
    final probe = FakeProbe(down: true);
    final mounts = await _pump(tester, probe, needsAgents: true);
    await tester.pump();
    expect(find.text('Waking up HexaGo...'), findsOneWidget);
    expect(find.byKey(const ValueKey('child')), findsNothing);
    expect(find.textContaining('DioException'), findsNothing);
    expect(find.textContaining('raw text'), findsNothing);
    expect(probe.agentsAsked.first, isTrue, reason: 'screens that need agents-py probe /v1/agents too');

    await tester.pump(const Duration(seconds: 5));
    await tester.pump();
    expect(probe.calls, 2);
    await tester.pump(const Duration(seconds: 5));
    await tester.pump();
    expect(probe.calls, 3);
    expect(find.byKey(const ValueKey('waking-attempts')), findsOneWidget);

    probe.down = false;
    await tester.pump(const Duration(seconds: 5));
    await tester.pump();
    expect(find.byKey(const ValueKey('child')), findsOneWidget);
    expect(find.byKey(const ValueKey('waking-screen')), findsNothing);
    expect(mounts, hasLength(2), reason: 'the screen is rebuilt so it reloads its data');

    // No more probes once awake.
    final calls = probe.calls;
    await tester.pump(const Duration(seconds: 30));
    expect(probe.calls, calls);
  });

  testWidgets('gives up after 2 minutes with a Retry button', (tester) async {
    final probe = FakeProbe(down: true);
    await _pump(tester, probe);
    await tester.pump();
    for (var i = 0; i < 25; i++) {
      await tester.pump(const Duration(seconds: 5));
    }
    await tester.pump();
    expect(find.byKey(const ValueKey('waking-retry')), findsOneWidget);
    expect(find.text('HexaGo is taking longer than usual'), findsOneWidget);
    final calls = probe.calls;
    expect(calls, inInclusiveRange(24, 26), reason: 'one probe every 5 s for 2 minutes');
    await tester.pump(const Duration(minutes: 1));
    expect(probe.calls, calls, reason: 'no probing after giving up');

    probe.down = false;
    await tester.tap(find.byKey(const ValueKey('waking-retry')));
    await tester.pump();
    await tester.pump();
    expect(find.byKey(const ValueKey('child')), findsOneWidget);
  });

  testWidgets('a slow first check shows the waking screen after the grace period', (tester) async {
    final probe = FakeProbe()..hold = Completer<void>();
    await _pump(tester, probe);
    expect(find.byKey(const ValueKey('child')), findsOneWidget);
    await tester.pump(const Duration(milliseconds: 1600));
    expect(find.text('Waking up HexaGo...'), findsOneWidget);
    probe.hold!.complete();
    await tester.pump();
    await tester.pump();
    expect(find.byKey(const ValueKey('child')), findsOneWidget);
  });

  testWidgets('a screen reporting a wake-up error switches to the waking screen', (tester) async {
    final probe = FakeProbe();
    await _pump(tester, probe);
    await tester.pump();
    probe.down = true;
    await tester.tap(find.byKey(const ValueKey('fail-other')));
    await tester.pump();
    expect(find.byKey(const ValueKey('child')), findsOneWidget, reason: 'other errors are the screen\'s to show');

    await tester.tap(find.byKey(const ValueKey('fail')));
    await tester.pump();
    await tester.pump();
    expect(find.text('Waking up HexaGo...'), findsOneWidget);
    probe.down = false;
    await tester.pump(const Duration(seconds: 5));
    await tester.pump();
    expect(find.byKey(const ValueKey('child')), findsOneWidget);
  });

  testWidgets('report outside a gate is a no-op', (tester) async {
    late BuildContext ctx;
    await tester.pumpWidget(
      MaterialApp(
        home: Builder(
          builder: (c) {
            ctx = c;
            return const SizedBox();
          },
        ),
      ),
    );
    expect(
      WakingGate.report(
        ctx,
        DioException(
          requestOptions: RequestOptions(path: '/'),
          type: DioExceptionType.connectionError,
        ),
      ),
      isFalse,
    );
  });
}
