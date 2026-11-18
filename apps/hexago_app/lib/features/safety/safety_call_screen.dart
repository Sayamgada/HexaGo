import 'dart:async';
import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../theme/hexago_theme.dart';
import 'data/safety_api.dart';
import 'models/safety_models.dart';
import 'state/safety_providers.dart';
import 'voice/voice_io.dart';

enum CallPhase { loading, ringing, sending, safe, help, timedOut, closed, error }

/// "HexaGo Safety" incoming call (route `/safety-call/:checkinId`).
///
/// The agent's question is spoken (flutter_tts); the rider answers by voice
/// (speech_to_text, where the browser/OS supports it) or by typing - the text
/// box and the quick replies are always there. A countdown shows how long is
/// left before the Safety Sentinel escalates on its own.
class SafetyCallScreen extends ConsumerStatefulWidget {
  const SafetyCallScreen({
    super.key,
    required this.checkinId,
    this.compact = false,
    this.onClose,
    this.now,
    this.tick = const Duration(milliseconds: 250),
    this.pollEvery = const Duration(seconds: 2),
  });

  final String checkinId;

  /// Phone-sized dialog on the Ops console ("Open rider phone").
  final bool compact;
  final VoidCallback? onClose;
  final DateTime Function()? now;
  final Duration tick;
  final Duration pollEvery;

  @override
  ConsumerState<SafetyCallScreen> createState() => _SafetyCallScreenState();
}

class _SafetyCallScreenState extends ConsumerState<SafetyCallScreen> {
  late final SafetyApi _api;
  late final VoiceIO _voice;
  final _text = TextEditingController();
  CallPhase _phase = CallPhase.loading;
  SafetyCall? _call;
  ReplyResult? _result;
  String _status = '';
  String? _error;
  DateTime? _deadline;
  double _remaining = 0;
  bool _canListen = false;
  bool _listening = false;
  Timer? _ticker;
  Timer? _poll;
  int _polls = 0;

  DateTime _now() => (widget.now ?? DateTime.now)();

  @override
  void initState() {
    super.initState();
    _api = ref.read(safetyApiProvider);
    _voice = ref.read(voiceProvider);
    unawaited(_load());
  }

  @override
  void dispose() {
    _ticker?.cancel();
    _poll?.cancel();
    unawaited(_voice.stopSpeaking());
    if (_listening) unawaited(_voice.stopListening());
    _text.dispose();
    super.dispose();
  }

  Future<void> _load() async {
    try {
      final call = await _api.getCheckin(widget.checkinId);
      if (!mounted) return;
      _call = call;
      if (!call.pending) {
        setState(() {
          _phase = CallPhase.closed;
          _status = _describeStatus(call.incidentStatus, call.status);
        });
        return;
      }
      setState(() {
        _phase = CallPhase.ringing;
        _remaining = call.remainingS;
        _deadline = _now().add(Duration(milliseconds: (call.remainingS * 1000).round()));
      });
      _ticker = Timer.periodic(widget.tick, (_) => _onTick());
      unawaited(_voice.speak(call.question));
      final ok = await _voice.initSpeech();
      if (mounted) setState(() => _canListen = ok);
    } catch (e) {
      if (mounted) {
        setState(() {
          _phase = CallPhase.error;
          _error = 'Could not reach HexaGo Safety: $e';
        });
      }
    }
  }

  void _onTick() {
    final d = _deadline;
    if (d == null || !mounted) return;
    final left = d.difference(_now()).inMilliseconds / 1000;
    setState(() => _remaining = math.max(0, left));
    if (left <= 0 && _phase == CallPhase.ringing) {
      _ticker?.cancel();
      if (_listening) unawaited(_voice.stopListening());
      setState(() {
        _phase = CallPhase.timedOut;
        _listening = false;
        _status = 'No answer - HexaGo Safety is escalating to emergency services.';
      });
      _poll = Timer.periodic(widget.pollEvery, (_) => _pollStatus());
    }
  }

  Future<void> _pollStatus() async {
    if (++_polls > 10) {
      _poll?.cancel();
      return;
    }
    try {
      final call = await _api.getCheckin(widget.checkinId);
      if (!mounted) return;
      if (call.incidentStatus == 'escalated') {
        _poll?.cancel();
        setState(() => _status = 'Escalated: help is on the way. Your live location is being shared.');
      }
    } catch (_) {/* keep the last message */}
  }

  String _describeStatus(String incident, String checkin) => switch (incident) {
        'escalated' => 'This call was escalated: help is on the way.',
        'monitoring' => 'You told us you are safe. We keep monitoring the trip.',
        'resolved' => 'This incident is resolved.',
        _ => 'This call is no longer active ($checkin).',
      };

  Future<void> _send(String text) async {
    final t = text.trim();
    if (t.isEmpty || _phase != CallPhase.ringing) return;
    _ticker?.cancel();
    if (_listening) {
      unawaited(_voice.stopListening());
      _listening = false;
    }
    unawaited(_voice.stopSpeaking());
    setState(() => _phase = CallPhase.sending);
    try {
      final r = await _api.reply(widget.checkinId, t);
      if (!mounted) return;
      setState(() {
        _result = r;
        _phase = r.escalated ? CallPhase.help : CallPhase.safe;
        _status = r.escalated
            ? 'Help is on the way. Emergency services have been notified and your live location is shared.'
            : "Thanks - glad you're safe. We'll keep an eye on your trip.";
      });
      unawaited(_voice.speak(_status));
    } on CheckinClosedException catch (e) {
      if (!mounted) return;
      setState(() {
        _phase = CallPhase.closed;
        _status = _describeStatus(e.call.incidentStatus, e.call.status);
      });
    } catch (e) {
      if (!mounted) return;
      setState(() {
        _phase = CallPhase.ringing;
        _error = 'Could not send your answer, please try again ($e)';
      });
      _ticker = Timer.periodic(widget.tick, (_) => _onTick());
    }
  }

  Future<void> _toggleMic() async {
    if (_listening) {
      await _voice.stopListening();
      if (mounted) setState(() => _listening = false);
      return;
    }
    await _voice.stopSpeaking();
    setState(() => _listening = true);
    await _voice.listen((words, isFinal) {
      if (!mounted) return;
      setState(() => _text.text = words);
      if (isFinal && words.trim().isNotEmpty) {
        _listening = false;
        unawaited(_send(words));
      }
    });
  }

  @override
  Widget build(BuildContext context) {
    final body = Container(
      key: const ValueKey('safety-call-screen'),
      decoration: const BoxDecoration(
        gradient: LinearGradient(
          begin: Alignment.topCenter,
          end: Alignment.bottomCenter,
          colors: [Color(0xFF1B0B16), HexaColors.background],
        ),
      ),
      child: SafeArea(
        child: Padding(
          padding: EdgeInsets.symmetric(horizontal: widget.compact ? 16 : 24, vertical: 16),
          child: Column(children: [
            Row(children: [
              const Icon(Icons.lock_outline, size: 14, color: HexaColors.textMuted),
              const SizedBox(width: 6),
              const Expanded(
                child: Text('Encrypted HexaGo call', style: TextStyle(color: HexaColors.textMuted, fontSize: 12)),
              ),
              if (widget.onClose != null)
                IconButton(
                  key: const ValueKey('safety-call-close'),
                  tooltip: 'Close',
                  onPressed: widget.onClose,
                  icon: const Icon(Icons.close, size: 18),
                ),
            ]),
            const SizedBox(height: 12),
            _Caller(phase: _phase),
            const SizedBox(height: 16),
            Expanded(child: SingleChildScrollView(child: _content())),
            if (_phase == CallPhase.ringing || _phase == CallPhase.sending) _answerBar(),
          ]),
        ),
      ),
    );
    return Scaffold(backgroundColor: HexaColors.background, body: body);
  }

  Widget _content() {
    switch (_phase) {
      case CallPhase.loading:
        return const Padding(padding: EdgeInsets.only(top: 40), child: Center(child: CircularProgressIndicator()));
      case CallPhase.error:
        return _Notice(key: const ValueKey('safety-call-error'), icon: Icons.wifi_off, color: HexaColors.bad, text: _error ?? '');
      case CallPhase.ringing:
      case CallPhase.sending:
        final call = _call!;
        return Column(children: [
          Container(
            key: const ValueKey('safety-question'),
            padding: const EdgeInsets.all(14),
            decoration: BoxDecoration(
              color: HexaColors.panel,
              borderRadius: BorderRadius.circular(14),
              border: Border.all(color: HexaColors.border),
            ),
            child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
              const Icon(Icons.record_voice_over_outlined, color: HexaColors.amber, size: 20),
              const SizedBox(width: 10),
              Expanded(child: Text(call.question, style: const TextStyle(fontSize: 15, height: 1.35))),
              IconButton(
                key: const ValueKey('safety-replay-question'),
                tooltip: 'Repeat the question',
                onPressed: () => _voice.speak(call.question),
                icon: const Icon(Icons.volume_up_outlined, size: 18),
              ),
            ]),
          ),
          const SizedBox(height: 18),
          _Countdown(remaining: _remaining, total: call.timeoutS),
          if (_error != null) ...[
            const SizedBox(height: 10),
            Text(_error!, style: const TextStyle(color: HexaColors.bad, fontSize: 12)),
          ],
        ]);
      case CallPhase.safe:
        return _Notice(
            key: const ValueKey('safety-call-safe'), icon: Icons.verified_user_outlined, color: HexaColors.good, text: _status,
            detail: 'We heard: "${_text.text.isEmpty ? '' : _text.text}"');
      case CallPhase.help:
        return _Notice(
            key: const ValueKey('safety-call-help'), icon: Icons.local_hospital_outlined, color: HexaColors.bad, text: _status,
            detail: _result == null ? null : 'Detected: ${_result!.matched.isEmpty ? _result!.label : _result!.matched.join(', ')}');
      case CallPhase.timedOut:
        return _Notice(key: const ValueKey('safety-call-timeout'), icon: Icons.sos_outlined, color: HexaColors.bad, text: _status);
      case CallPhase.closed:
        return _Notice(key: const ValueKey('safety-call-closed'), icon: Icons.info_outline, color: HexaColors.cyan, text: _status);
    }
  }

  Widget _answerBar() {
    final busy = _phase == CallPhase.sending;
    return Column(mainAxisSize: MainAxisSize.min, children: [
      Row(children: [
        Expanded(
          child: OutlinedButton.icon(
            key: const ValueKey('quick-fine'),
            onPressed: busy ? null : () => _send("I'm fine"),
            style: OutlinedButton.styleFrom(foregroundColor: HexaColors.good, side: const BorderSide(color: HexaColors.good)),
            icon: const Icon(Icons.thumb_up_alt_outlined, size: 16),
            label: const Text("I'm fine"),
          ),
        ),
        const SizedBox(width: 10),
        Expanded(
          child: FilledButton.icon(
            key: const ValueKey('quick-help'),
            onPressed: busy ? null : () => _send('I need help'),
            style: FilledButton.styleFrom(backgroundColor: HexaColors.bad, foregroundColor: Colors.white),
            icon: const Icon(Icons.sos, size: 16),
            label: const Text('I need help'),
          ),
        ),
      ]),
      const SizedBox(height: 10),
      Row(children: [
        if (_canListen) ...[
          IconButton.filled(
            key: const ValueKey('safety-mic'),
            tooltip: _listening ? 'Stop listening' : 'Answer by voice',
            style: IconButton.styleFrom(backgroundColor: _listening ? HexaColors.bad : HexaColors.amber),
            onPressed: busy ? null : _toggleMic,
            icon: Icon(_listening ? Icons.mic : Icons.mic_none, color: Colors.black),
          ),
          const SizedBox(width: 8),
        ],
        Expanded(
          child: TextField(
            key: const ValueKey('safety-reply-field'),
            controller: _text,
            enabled: !busy,
            textInputAction: TextInputAction.send,
            onSubmitted: _send,
            decoration: InputDecoration(
              isDense: true,
              hintText: _listening ? 'Listening...' : 'Type your answer',
              filled: true,
              fillColor: HexaColors.panel,
              border: OutlineInputBorder(borderRadius: BorderRadius.circular(24)),
            ),
          ),
        ),
        const SizedBox(width: 8),
        IconButton(
          key: const ValueKey('safety-send'),
          tooltip: 'Send',
          onPressed: busy ? null : () => _send(_text.text),
          icon: busy
              ? const SizedBox(width: 18, height: 18, child: CircularProgressIndicator(strokeWidth: 2))
              : const Icon(Icons.send),
        ),
      ]),
    ]);
  }
}

class _Caller extends StatelessWidget {
  const _Caller({required this.phase});
  final CallPhase phase;

  @override
  Widget build(BuildContext context) {
    final live = phase == CallPhase.ringing || phase == CallPhase.loading;
    return Column(children: [
      Container(
        width: 84,
        height: 84,
        decoration: BoxDecoration(
          shape: BoxShape.circle,
          color: HexaColors.incident.withValues(alpha: 0.18),
          border: Border.all(color: HexaColors.incident, width: live ? 3 : 1.5),
        ),
        child: const Icon(Icons.health_and_safety, size: 42, color: HexaColors.incident),
      ),
      const SizedBox(height: 10),
      const Text('HexaGo Safety', style: TextStyle(fontSize: 22, fontWeight: FontWeight.w700)),
      const SizedBox(height: 2),
      Text(live ? 'Incoming safety call' : 'Call ended',
          style: const TextStyle(color: HexaColors.textMuted, fontSize: 13)),
    ]);
  }
}

class _Countdown extends StatelessWidget {
  const _Countdown({required this.remaining, required this.total});
  final double remaining;
  final double total;

  @override
  Widget build(BuildContext context) {
    final secs = remaining.ceil();
    final frac = total <= 0 ? 0.0 : (remaining / total).clamp(0.0, 1.0);
    final urgent = remaining <= 5;
    return Column(key: const ValueKey('safety-countdown'), children: [
      SizedBox(
        width: 70,
        height: 70,
        child: Stack(alignment: Alignment.center, children: [
          SizedBox.expand(
            child: CircularProgressIndicator(
              value: frac,
              strokeWidth: 5,
              color: urgent ? HexaColors.bad : HexaColors.amber,
              backgroundColor: HexaColors.border,
            ),
          ),
          Text('$secs', style: TextStyle(fontSize: 24, fontWeight: FontWeight.w700, color: urgent ? HexaColors.bad : null)),
        ]),
      ),
      const SizedBox(height: 6),
      Text('Answer within $secs s of ${total.round()} s, or we alert emergency services',
          textAlign: TextAlign.center, style: const TextStyle(color: HexaColors.textMuted, fontSize: 12)),
    ]);
  }
}

class _Notice extends StatelessWidget {
  const _Notice({super.key, required this.icon, required this.color, required this.text, this.detail});
  final IconData icon;
  final Color color;
  final String text;
  final String? detail;

  @override
  Widget build(BuildContext context) {
    return Container(
      margin: const EdgeInsets.only(top: 12),
      padding: const EdgeInsets.all(16),
      decoration: BoxDecoration(
        color: color.withValues(alpha: 0.12),
        borderRadius: BorderRadius.circular(14),
        border: Border.all(color: color.withValues(alpha: 0.6)),
      ),
      child: Column(children: [
        Icon(icon, color: color, size: 36),
        const SizedBox(height: 10),
        Text(text, textAlign: TextAlign.center, style: const TextStyle(fontSize: 15, height: 1.35)),
        if (detail != null) ...[
          const SizedBox(height: 8),
          Text(detail!, textAlign: TextAlign.center, style: const TextStyle(color: HexaColors.textMuted, fontSize: 12)),
        ],
      ]),
    );
  }
}

/// Opens the rider's call screen in a phone-sized dialog (Ops console demo).
Future<void> showRiderPhone(BuildContext context, String checkinId) {
  return showDialog<void>(
    context: context,
    builder: (ctx) => Dialog(
      key: const ValueKey('rider-phone-dialog'),
      insetPadding: const EdgeInsets.all(12),
      clipBehavior: Clip.antiAlias,
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(28), side: const BorderSide(color: HexaColors.border, width: 6)),
      child: SizedBox(
        width: 380,
        height: math.min(760, MediaQuery.sizeOf(ctx).height - 40),
        child: SafetyCallScreen(checkinId: checkinId, compact: true, onClose: () => Navigator.of(ctx).pop()),
      ),
    ),
  );
}
