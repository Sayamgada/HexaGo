import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../features/platform/state/platform_providers.dart';
import '../theme/hexago_theme.dart';
import '../util/errors.dart';
import 'hexago_logo.dart';

/// Checks that the backend is up: `GET /healthz`, plus `GET /v1/agents` when
/// [agents] (screens that need agents-py). Throws while it is asleep.
typedef WakeProbe = Future<void> Function({required bool agents});

final wakeProbeProvider = Provider<WakeProbe>((ref) {
  final api = ref.watch(platformApiProvider);
  return ({required bool agents}) => api.probe(agents: agents);
});

/// Shows [child] while the backend answers. When the first check is slow or
/// fails, or the screen reports a wake-up error (502/503, no connection,
/// `agents_waking`) through [WakingGate.report], it shows a friendly
/// "Waking up HexaGo..." screen that re-checks every [retryEvery] for up to
/// [giveUpAfter], then offers a Retry button. Once the backend answers, the
/// child is rebuilt from scratch so it reloads its data.
class WakingGate extends ConsumerStatefulWidget {
  const WakingGate({
    super.key,
    required this.child,
    this.needsAgents = false,
    this.retryEvery = const Duration(seconds: 5),
    this.giveUpAfter = const Duration(minutes: 2),
    this.graceBeforeShowing = const Duration(milliseconds: 1500),
  });

  final Widget child;
  final bool needsAgents;
  final Duration retryEvery;
  final Duration giveUpAfter;

  /// The first check may take this long before the waking screen appears.
  final Duration graceBeforeShowing;

  /// Lets a screen hand a failed request to the nearest gate. Returns true
  /// when the error was a wake-up error (the gate is now showing).
  static bool report(BuildContext context, Object error) {
    if (!isWakeError(error)) return false;
    final state = context.findAncestorStateOfType<_WakingGateState>();
    if (state == null) return false;
    state._startWaking();
    return true;
  }

  @override
  ConsumerState<WakingGate> createState() => _WakingGateState();
}

enum _Phase { ready, waking, gaveUp }

class _WakingGateState extends ConsumerState<WakingGate> {
  _Phase _phase = _Phase.ready;
  Timer? _grace;
  Timer? _retry;
  int _ticks = 0; // retry periods elapsed while waking
  int _attempts = 0;
  int _generation = 0; // bumps the child's key so it reloads after a wake-up
  bool _checking = false;

  @override
  void initState() {
    super.initState();
    _grace = Timer(widget.graceBeforeShowing, () {
      if (_checking && _phase == _Phase.ready) _startWaking(probeNow: false);
    });
    unawaited(_check());
  }

  @override
  void dispose() {
    _grace?.cancel();
    _retry?.cancel();
    super.dispose();
  }

  Future<void> _check() async {
    if (_checking) return;
    _checking = true;
    _attempts++;
    try {
      await ref.read(wakeProbeProvider)(agents: widget.needsAgents);
      if (!mounted) return;
      _retry?.cancel();
      _grace?.cancel();
      if (_phase != _Phase.ready) {
        setState(() {
          _phase = _Phase.ready;
          _generation++;
        });
      }
    } catch (_) {
      if (mounted && _phase == _Phase.ready) _startWaking(probeNow: false);
    } finally {
      _checking = false;
    }
  }

  void _startWaking({bool probeNow = true}) {
    if (!mounted || _phase == _Phase.waking) return;
    _grace?.cancel();
    _retry?.cancel();
    setState(() {
      _phase = _Phase.waking;
      _ticks = 0;
      _attempts = 0;
    });
    _retry = Timer.periodic(widget.retryEvery, (_) {
      _ticks++;
      if (widget.retryEvery * _ticks > widget.giveUpAfter) {
        _retry?.cancel();
        if (mounted && _phase == _Phase.waking) setState(() => _phase = _Phase.gaveUp);
        return;
      }
      unawaited(
        _check().then((_) {
          if (mounted && _phase == _Phase.waking) setState(() {}); // attempt counter
        }),
      );
    });
    if (probeNow) unawaited(_check());
  }

  @override
  Widget build(BuildContext context) {
    final child = KeyedSubtree(key: ValueKey('woken-$_generation'), child: widget.child);
    if (_phase == _Phase.ready) return child;
    return WakingScreen(
      gaveUp: _phase == _Phase.gaveUp,
      attempts: _attempts,
      onRetry: () {
        setState(() => _phase = _Phase.ready); // _startWaking ignores an already-waking gate
        _startWaking();
      },
    );
  }
}

/// "Waking up HexaGo..." with a spinner, or a Retry button after the window.
class WakingScreen extends StatelessWidget {
  const WakingScreen({super.key, required this.gaveUp, this.attempts = 0, this.onRetry});

  final bool gaveUp;
  final int attempts;
  final VoidCallback? onRetry;

  @override
  Widget build(BuildContext context) {
    return Material(
      key: const ValueKey('waking-screen'),
      color: HexaColors.background,
      child: SafeArea(
        child: Center(
          child: Padding(
            padding: const EdgeInsets.all(28),
            child: ConstrainedBox(
              constraints: const BoxConstraints(maxWidth: 420),
              child: Column(
                mainAxisSize: MainAxisSize.min,
                children: [
                  const HexaGoLogo(size: 64),
                  const SizedBox(height: 20),
                  Text(
                    gaveUp ? 'HexaGo is taking longer than usual' : 'Waking up HexaGo...',
                    key: const ValueKey('waking-title'),
                    textAlign: TextAlign.center,
                    style: const TextStyle(fontSize: 22, fontWeight: FontWeight.w800),
                  ),
                  const SizedBox(height: 10),
                  Text(
                    gaveUp
                        ? 'The demo servers did not answer within 2 minutes. They may still be starting.'
                        : 'The demo servers sleep when nobody is using them and take up to a minute to start. '
                              'This page continues on its own.',
                    textAlign: TextAlign.center,
                    style: const TextStyle(color: HexaColors.textMuted, height: 1.4),
                  ),
                  const SizedBox(height: 22),
                  if (gaveUp)
                    FilledButton.icon(
                      key: const ValueKey('waking-retry'),
                      onPressed: onRetry,
                      icon: const Icon(Icons.refresh),
                      label: const Text('Retry'),
                    )
                  else ...[
                    const SizedBox(width: 28, height: 28, child: CircularProgressIndicator(strokeWidth: 3)),
                    if (attempts > 1) ...[
                      const SizedBox(height: 12),
                      Text(
                        'Still starting... (check $attempts)',
                        key: const ValueKey('waking-attempts'),
                        style: const TextStyle(fontSize: 12, color: HexaColors.textMuted),
                      ),
                    ],
                  ],
                ],
              ),
            ),
          ),
        ),
      ),
    );
  }
}
