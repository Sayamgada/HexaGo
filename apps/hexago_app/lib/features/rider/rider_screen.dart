import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../config/app_config.dart';
import '../../theme/hexago_theme.dart';
import '../safety/state/safety_providers.dart' show voiceProvider;
import '../safety/voice/voice_io.dart';
import 'data/booking_api.dart';
import 'models/booking_models.dart';
import 'state/rider_providers.dart';
import 'widgets/plan_timeline.dart';
import 'widgets/scheduled_legs.dart';

sealed class _Item {}

class _UserSaid extends _Item {
  _UserSaid(this.text);
  final String text;
}

class _AgentAsked extends _Item {
  _AgentAsked(this.q);
  final ClarifyingQuestion q;
}

class _AgentPlanned extends _Item {
  _AgentPlanned(this.plan);
  final BookingPlan plan;
}

class _Note extends _Item {
  _Note(this.text, {this.error = false});
  final String text;
  final bool error;
}

/// Rider module (route `/rider`): persona picker, a big chat / voice box for
/// the whole plan, the plan timeline with Edit / Confirm, then the scheduled
/// legs and a live map of each leg once dispatch-go dispatches it.
class RiderScreen extends ConsumerStatefulWidget {
  const RiderScreen({
    super.key,
    this.initialPersonaId,
    this.pollEvery = const Duration(seconds: 3),
    this.showLiveMap = true,
    this.now,
  });

  final String? initialPersonaId;
  final Duration pollEvery;
  final bool showLiveMap;
  final DateTime Function()? now;

  @override
  ConsumerState<RiderScreen> createState() => _RiderScreenState();
}

class _RiderScreenState extends ConsumerState<RiderScreen> {
  late final BookingApi _api;
  late final VoiceIO _voice;
  final _text = TextEditingController();
  final _scroll = ScrollController();
  final _items = <_Item>[];
  final _streams = <String, Stream<RideUpdate>>{};

  List<Persona> _personas = const [];
  Persona? _persona;
  String? _loadError;
  bool _planning = false;
  DateTime? _planningSince;
  String? _replyTo;
  String? _confirmingPlan;
  String? _confirmedPlan;
  List<ScheduledLeg> _scheduled = const [];
  bool _forceFallback = false;
  bool _canListen = false;
  bool _listening = false;
  bool _fromVoice = false;
  Timer? _poll;
  Timer? _clock;

  DateTime _now() => (widget.now ?? DateTime.now)();

  @override
  void initState() {
    super.initState();
    _api = ref.read(bookingApiProvider);
    _voice = ref.read(voiceProvider);
    unawaited(_loadPersonas());
    unawaited(_voice.initSpeech().then((ok) {
      if (mounted) setState(() => _canListen = ok);
    }));
  }

  @override
  void dispose() {
    _poll?.cancel();
    _clock?.cancel();
    if (_listening) unawaited(_voice.stopListening());
    _text.dispose();
    _scroll.dispose();
    super.dispose();
  }

  Future<void> _loadPersonas() async {
    try {
      final list = await _api.personas();
      if (!mounted) return;
      setState(() {
        _personas = list;
        _persona = list.where((p) => p.id == widget.initialPersonaId).firstOrNull;
      });
    } catch (e) {
      if (mounted) setState(() => _loadError = 'Could not reach HexaGo: $e');
    }
  }

  void _pickPersona(Persona p) => setState(() {
        _persona = p;
        _items.clear();
        _replyTo = null;
        _scheduled = const [];
        _confirmedPlan = null;
        _poll?.cancel();
      });

  // ---- sending ----
  Future<void> _send([String? override]) async {
    final text = (override ?? _text.text).trim();
    final persona = _persona;
    if (text.isEmpty || persona == null || _planning) return;
    if (_listening) await _toggleVoice();
    setState(() {
      _items.add(_UserSaid(text));
      _planning = true;
      _planningSince = _now();
      _fromVoice = false;
      if (override == null) _text.clear();
    });
    _clock = Timer.periodic(const Duration(seconds: 1), (_) => mounted ? setState(() {}) : null);
    _scrollDown();
    final replyTo = _replyTo;
    try {
      final reply = await _api.parse(persona.id, text, replyTo: replyTo, forceFallback: _forceFallback);
      if (!mounted) return;
      setState(() {
        switch (reply) {
          case ClarifyingQuestion q:
            _replyTo = q.questionId;
            _items.add(_AgentAsked(q));
          case BookingPlan p:
            _replyTo = null;
            _items.add(_AgentPlanned(p));
        }
      });
    } catch (e) {
      if (mounted) setState(() => _items.add(_Note('Planning failed: $e', error: true)));
    } finally {
      _clock?.cancel();
      if (mounted) setState(() => _planning = false);
      _scrollDown();
    }
  }

  void _scrollDown() => WidgetsBinding.instance.addPostFrameCallback((_) {
        if (_scroll.hasClients) {
          _scroll.animateTo(_scroll.position.maxScrollExtent,
              duration: const Duration(milliseconds: 250), curve: Curves.easeOut);
        }
      });

  void _edit(BookingPlan plan) => setState(() {
        _items.removeWhere((i) => i is _AgentPlanned && i.plan.planId == plan.planId);
        _text.text = plan.text;
        _replyTo = null;
      });

  Future<void> _confirm(BookingPlan plan) async {
    setState(() => _confirmingPlan = plan.planId);
    try {
      final res = await _api.confirm(plan.planId);
      if (!mounted) return;
      setState(() {
        _confirmedPlan = plan.planId;
        _items.add(_Note(res.demoMode
            ? 'Scheduled ${res.scheduledIds.length} rides. Demo time: the first leg dispatches in 20 s, '
                'each next leg 60 s after the previous one completes.'
            : 'Scheduled ${res.scheduledIds.length} rides; each dispatches shortly before its pickup time.'));
      });
      await _refreshScheduled();
      _poll?.cancel();
      _poll = Timer.periodic(widget.pollEvery, (_) => _refreshScheduled());
    } catch (e) {
      if (mounted) setState(() => _items.add(_Note('Could not schedule the rides: $e', error: true)));
    } finally {
      if (mounted) setState(() => _confirmingPlan = null);
      _scrollDown();
    }
  }

  Future<void> _refreshScheduled() async {
    final persona = _persona;
    if (persona == null) return;
    try {
      final legs = await _api.scheduled(persona.id);
      if (!mounted) return;
      final mine = legs.where((l) => l.planId == _confirmedPlan).toList()..sort((a, b) => a.seq.compareTo(b.seq));
      setState(() => _scheduled = mine);
      if (mine.isNotEmpty && mine.every((l) => l.finished)) _poll?.cancel();
    } catch (_) {
      // keep the last good list; the next poll retries
    }
  }

  // ---- voice ----
  Future<void> _toggleVoice() async {
    if (_listening) {
      await _voice.stopListening();
      if (mounted) setState(() => _listening = false);
      return;
    }
    setState(() {
      _listening = true;
      _fromVoice = true;
    });
    await _voice.listen((words, isFinal) {
      if (!mounted) return;
      setState(() {
        _text.text = words; // always shown before sending; the rider presses send
        if (isFinal) _listening = false;
      });
    });
  }

  // ---- UI ----
  @override
  Widget build(BuildContext context) {
    final persona = _persona;
    return Scaffold(
      appBar: AppBar(
        title: const Text('HexaGo Rider'),
        actions: [
          if (persona != null)
            Padding(
              padding: const EdgeInsets.only(right: 8),
              child: ActionChip(
                key: const Key('persona-chip'),
                avatar: const Icon(Icons.person, size: 16),
                label: Text(persona.name),
                onPressed: () => setState(() => _persona = null),
              ),
            ),
          PopupMenuButton<String>(
            key: const Key('rider-menu'),
            onSelected: (_) => setState(() => _forceFallback = !_forceFallback),
            itemBuilder: (_) => [
              CheckedPopupMenuItem(value: 'fallback', checked: _forceFallback,
                  child: const Text('Force rule-based parser (demo)')),
            ],
          ),
        ],
      ),
      body: Center(
        child: ConstrainedBox(
          constraints: const BoxConstraints(maxWidth: 820),
          child: persona == null ? _picker() : _booking(persona),
        ),
      ),
    );
  }

  Widget _picker() {
    if (_loadError != null) {
      return Padding(padding: const EdgeInsets.all(24), child: Text(_loadError!, key: const Key('rider-error')));
    }
    if (_personas.isEmpty) return const Center(child: CircularProgressIndicator());
    return ListView(padding: const EdgeInsets.all(20), children: [
      const Text('Who is riding today?', style: TextStyle(fontSize: 22, fontWeight: FontWeight.bold)),
      const SizedBox(height: 4),
      Text('Demo rider personas in ${AppConfig.defaultCity}, each with saved places.',
          style: const TextStyle(color: HexaColors.textMuted)),
      const SizedBox(height: 16),
      for (final p in _personas)
        Card(
          color: HexaColors.panel,
          child: ListTile(
            key: Key('persona-${p.id}'),
            leading: const CircleAvatar(child: Icon(Icons.person)),
            title: Text(p.name),
            subtitle: Text(p.blurb),
            trailing: const Icon(Icons.chevron_right),
            onTap: () => _pickPersona(p),
          ),
        ),
    ]);
  }

  Widget _booking(Persona persona) {
    return Column(children: [
      _inputBox(persona),
      Expanded(
        child: ListView(controller: _scroll, padding: const EdgeInsets.fromLTRB(16, 4, 16, 24), children: [
          if (_items.isEmpty) _hint(persona),
          for (final item in _items) _render(item),
          if (_planning) _planningCard(),
          if (_scheduled.isNotEmpty) _scheduledPanel(),
        ]),
      ),
    ]);
  }

  Widget _inputBox(Persona persona) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(16, 16, 16, 8),
      child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
        TextField(
          key: const Key('booking-input'),
          controller: _text,
          minLines: 2,
          maxLines: 5,
          textInputAction: TextInputAction.send,
          onSubmitted: (_) => _send(),
          style: const TextStyle(fontSize: 17),
          decoration: InputDecoration(
            hintText: _replyTo != null ? 'Type your answer…' : 'Where to? Or describe your whole plan...',
            filled: true,
            fillColor: HexaColors.surface,
            border: OutlineInputBorder(borderRadius: BorderRadius.circular(14)),
            suffixIcon: Row(mainAxisSize: MainAxisSize.min, children: [
              if (_canListen)
                IconButton(
                  key: const Key('booking-mic'),
                  tooltip: _listening ? 'Stop listening' : 'Speak your plan',
                  icon: Icon(_listening ? Icons.stop_circle : Icons.mic, color: _listening ? HexaColors.bad : null),
                  onPressed: _planning ? null : _toggleVoice,
                ),
              IconButton(
                key: const Key('booking-send'),
                tooltip: 'Plan it',
                icon: const Icon(Icons.send),
                onPressed: _planning ? null : () => _send(),
              ),
            ]),
          ),
        ),
        if (_fromVoice && _text.text.isNotEmpty)
          const Padding(
            padding: EdgeInsets.only(top: 4),
            child: Text('Voice transcript above: edit it if needed, then press send.',
                key: Key('voice-transcript-hint'), style: TextStyle(color: HexaColors.textMuted, fontSize: 12)),
          ),
        if (_forceFallback)
          const Padding(
            padding: EdgeInsets.only(top: 4),
            child: Text('Rule-based parser forced (demo)', style: TextStyle(color: Colors.amberAccent, fontSize: 12)),
          ),
      ]),
    );
  }

  Widget _hint(Persona persona) => Padding(
        padding: const EdgeInsets.only(top: 12),
        child: Text(
          'Hi ${persona.firstName}! Try: "Pick up my kid from school at 3:15 PM, drop them at soccer practice, '
          'and then book a high-tier sedan for me from my office to the airport to arrive by 6:00 PM."',
          style: const TextStyle(color: HexaColors.textMuted),
        ),
      );

  Widget _planningCard() {
    final secs = _planningSince == null ? 0 : _now().difference(_planningSince!).inSeconds;
    return Card(
      key: const Key('planning'),
      color: HexaColors.panel,
      child: ListTile(
        leading: const SizedBox(width: 22, height: 22, child: CircularProgressIndicator(strokeWidth: 2.5)),
        title: const Text('Planning your trip...'),
        subtitle: Text('${_forceFallback ? 'Rule-based parser' : 'AI planner'} · ${secs}s (can take up to ~12 s)'),
      ),
    );
  }

  Widget _bubble(String text, {bool user = false, bool error = false, Widget? extra}) => Align(
        alignment: user ? Alignment.centerRight : Alignment.centerLeft,
        child: Container(
          margin: const EdgeInsets.symmetric(vertical: 6),
          padding: const EdgeInsets.all(12),
          constraints: const BoxConstraints(maxWidth: 560),
          decoration: BoxDecoration(
            color: user ? HexaColors.amber.withValues(alpha: 0.18) : (error ? HexaColors.bad.withValues(alpha: 0.15) : HexaColors.panel),
            borderRadius: BorderRadius.circular(14),
          ),
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [Text(text), ?extra]),
        ),
      );

  Widget _render(_Item item) => switch (item) {
        _UserSaid(:final text) => _bubble(text, user: true),
        _Note(:final text, :final error) => _bubble(text, error: error),
        _AgentAsked(:final q) => _bubble(
            q.question,
            extra: Padding(
              padding: const EdgeInsets.only(top: 8),
              child: Wrap(spacing: 8, runSpacing: 6, children: [
                for (final o in q.options)
                  ActionChip(
                    key: Key('chip-$o'),
                    label: Text(o),
                    onPressed: _planning || _replyTo != q.questionId ? null : () => _send(o),
                  ),
                AnsweredByTag(q.answeredBy),
              ]),
            ),
          ),
        _AgentPlanned(:final plan) => PlanTimeline(
            plan: plan,
            confirming: _confirmingPlan == plan.planId,
            confirmed: _confirmedPlan == plan.planId,
            onConfirm: () => _confirm(plan),
            onEdit: () => _edit(plan),
          ),
      };

  Widget _scheduledPanel() {
    final live = _scheduled.where((l) => l.live).firstOrNull;
    return ScheduledLegsPanel(
      legs: _scheduled,
      now: _now(),
      showMap: widget.showLiveMap,
      live: live == null
          ? null
          : LiveRideMap(
              key: ValueKey(live.rideId),
              leg: live,
              updates: _streams.putIfAbsent(live.rideId!, () => _api.rideUpdates(live.rideId!).asBroadcastStream()),
            ),
    );
  }
}
