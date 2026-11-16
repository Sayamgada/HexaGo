import 'package:flutter/material.dart';

import '../../../theme/hexago_theme.dart';
import '../../../util/format.dart';
import '../../../widgets/hexagon.dart';
import '../models/ops_models.dart';

/// Visual identity per agent in the roster (PROJECT_CONTEXT.md §3).
class AgentStyle {
  const AgentStyle(this.icon, this.color);
  final IconData icon;
  final Color color;

  static const _byKey = <String, AgentStyle>{
    'bookingintent': AgentStyle(Icons.chat_bubble_outline, Color(0xFF60A5FA)),
    'dispatchbroker': AgentStyle(Icons.hub_outlined, Color(0xFFFFB020)),
    'poolbatching': AgentStyle(Icons.group_outlined, Color(0xFFA78BFA)),
    'surgeequilibrium': AgentStyle(Icons.trending_up, Color(0xFFF472B6)),
    'demandforecast': AgentStyle(Icons.insights, Color(0xFF2DD4BF)),
    'safetysentinel': AgentStyle(Icons.shield_outlined, Color(0xFFF43F5E)),
    'compliancevision': AgentStyle(Icons.document_scanner_outlined, Color(0xFF4ADE80)),
    'legaljurisdiction': AgentStyle(Icons.gavel, Color(0xFFFB923C)),
    'settlement': AgentStyle(Icons.payments_outlined, Color(0xFF34D399)),
    'supportdispute': AgentStyle(Icons.support_agent, Color(0xFF38BDF8)),
    'opssupervisor': AgentStyle(Icons.visibility_outlined, Color(0xFFE2E8F0)),
  };

  /// Normalises `dispatch_broker`, `Dispatch Broker`, `DispatchBrokerAgent`...
  static String keyFor(String agent) {
    var k = agent
        .replaceAllMapped(RegExp(r'([a-z])([A-Z])'), (m) => '${m[1]} ${m[2]}')
        .toLowerCase()
        .replaceAll(RegExp(r'(^|[^a-z])and([^a-z]|$)'), ' ')
        .replaceAll(RegExp(r'[^a-z]'), '');
    if (k.endsWith('agent') && k.length > 5) k = k.substring(0, k.length - 5);
    return k;
  }

  static AgentStyle of(String agent) =>
      _byKey[keyFor(agent)] ??
      AgentStyle(Icons.smart_toy_outlined, HSLColor.fromAHSL(1, (agent.hashCode % 360).toDouble().abs(), 0.6, 0.65).toColor());
}

/// `dispatch_broker` -> `Dispatch Broker`.
String displayAgentName(String agent) {
  if (agent.contains(' ') || !agent.contains('_')) return agent;
  return agent.split('_').where((p) => p.isNotEmpty).map((p) => p[0].toUpperCase() + p.substring(1)).join(' ');
}

class AgentFeed extends StatelessWidget {
  const AgentFeed({super.key, required this.events, this.totalSeen});

  final List<AgentEvent> events;
  final int? totalSeen;

  @override
  Widget build(BuildContext context) {
    return Container(
      decoration: BoxDecoration(
        color: HexaColors.surface,
        borderRadius: BorderRadius.circular(12),
        border: Border.all(color: HexaColors.border),
      ),
      child: Column(
        children: [
          Padding(
            padding: const EdgeInsets.fromLTRB(14, 12, 14, 8),
            child: Row(children: [
              const _PulseDot(),
              const SizedBox(width: 8),
              const Text('Live Agent Feed', style: TextStyle(fontWeight: FontWeight.w700, fontSize: 15)),
              const Spacer(),
              Text(
                '${formatCount(totalSeen ?? events.length)} events',
                key: const ValueKey('feed-count'),
                style: const TextStyle(fontSize: 11, color: HexaColors.textMuted),
              ),
            ]),
          ),
          const Divider(height: 1),
          Expanded(
            child: events.isEmpty
                ? const Center(
                    child: Text('Waiting for agent decisions…', style: TextStyle(color: HexaColors.textMuted)),
                  )
                : ListView.builder(
                    key: const ValueKey('agent-feed-list'),
                    padding: const EdgeInsets.symmetric(vertical: 4),
                    itemCount: events.length,
                    itemBuilder: (context, i) => AgentEventTile(key: ValueKey(events[i].id), event: events[i]),
                  ),
          ),
        ],
      ),
    );
  }
}

class AgentEventTile extends StatefulWidget {
  const AgentEventTile({super.key, required this.event});
  final AgentEvent event;

  @override
  State<AgentEventTile> createState() => _AgentEventTileState();
}

class _AgentEventTileState extends State<AgentEventTile> {
  bool _expanded = false;

  @override
  Widget build(BuildContext context) {
    final e = widget.event;
    final style = AgentStyle.of(e.agent);
    final confColor = e.confidence >= 0.85
        ? HexaColors.good
        : e.confidence >= 0.6
            ? HexaColors.warn
            : HexaColors.bad;

    return InkWell(
      onTap: () => setState(() => _expanded = !_expanded),
      child: Padding(
        padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 8),
        child: Row(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            HexBadge(size: 34, color: style.color, child: Icon(style.icon, size: 16, color: style.color)),
            const SizedBox(width: 10),
            Expanded(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Row(children: [
                    Flexible(
                      child: Text(
                        displayAgentName(e.agent),
                        overflow: TextOverflow.ellipsis,
                        style: TextStyle(color: style.color, fontWeight: FontWeight.w700, fontSize: 12),
                      ),
                    ),
                    if (e.usedFallback) ...[
                      const SizedBox(width: 6),
                      const _FallbackTag(),
                    ],
                    const Spacer(),
                    Text(formatClock(e.ts), style: const TextStyle(fontSize: 10, color: HexaColors.textMuted)),
                  ]),
                  const SizedBox(height: 3),
                  Text(
                    e.decision,
                    maxLines: _expanded ? null : 2,
                    overflow: _expanded ? null : TextOverflow.ellipsis,
                    style: const TextStyle(fontSize: 13, height: 1.25),
                  ),
                  const SizedBox(height: 5),
                  Row(children: [
                    Text(
                      '${(e.confidence * 100).round()}% conf',
                      key: const ValueKey('event-confidence'),
                      style: TextStyle(fontSize: 11, color: confColor, fontWeight: FontWeight.w600),
                    ),
                    const SizedBox(width: 6),
                    SizedBox(
                      width: 40,
                      child: ClipRRect(
                        borderRadius: BorderRadius.circular(2),
                        child: LinearProgressIndicator(
                          value: e.confidence,
                          minHeight: 3,
                          color: confColor,
                          backgroundColor: Colors.white10,
                        ),
                      ),
                    ),
                    const SizedBox(width: 10),
                    const Icon(Icons.timer_outlined, size: 11, color: HexaColors.textMuted),
                    const SizedBox(width: 2),
                    Text(
                      formatMs(e.latencyMs),
                      key: const ValueKey('event-latency'),
                      style: const TextStyle(fontSize: 11, color: HexaColors.textMuted),
                    ),
                    const Spacer(),
                    Icon(_expanded ? Icons.expand_less : Icons.expand_more, size: 16, color: HexaColors.textMuted),
                  ]),
                  AnimatedSize(
                    duration: const Duration(milliseconds: 150),
                    alignment: Alignment.topCenter,
                    child: _expanded && e.reasoning.isNotEmpty
                        ? Container(
                            key: const ValueKey('event-reasoning'),
                            width: double.infinity,
                            margin: const EdgeInsets.only(top: 6),
                            padding: const EdgeInsets.all(8),
                            decoration: BoxDecoration(
                              color: Colors.white.withValues(alpha: 0.04),
                              borderRadius: BorderRadius.circular(6),
                              border: Border(left: BorderSide(color: style.color, width: 2)),
                            ),
                            child: Text(
                              e.reasoning,
                              style: const TextStyle(fontSize: 12, color: Color(0xFFCBD5E1), height: 1.35),
                            ),
                          )
                        : const SizedBox(width: double.infinity),
                  ),
                ],
              ),
            ),
          ],
        ),
      ),
    );
  }
}

class _FallbackTag extends StatelessWidget {
  const _FallbackTag();

  @override
  Widget build(BuildContext context) {
    return Tooltip(
      message: 'Deterministic non-LLM fallback made this decision',
      child: Container(
        key: const ValueKey('fallback-tag'),
        padding: const EdgeInsets.symmetric(horizontal: 5, vertical: 1),
        decoration: BoxDecoration(
          color: HexaColors.warn.withValues(alpha: 0.15),
          borderRadius: BorderRadius.circular(4),
          border: Border.all(color: HexaColors.warn.withValues(alpha: 0.6)),
        ),
        child: const Text(
          'FALLBACK',
          style: TextStyle(fontSize: 9, fontWeight: FontWeight.w800, color: HexaColors.warn, letterSpacing: 0.6),
        ),
      ),
    );
  }
}

class _PulseDot extends StatefulWidget {
  const _PulseDot();

  @override
  State<_PulseDot> createState() => _PulseDotState();
}

class _PulseDotState extends State<_PulseDot> with SingleTickerProviderStateMixin {
  late final AnimationController _c = AnimationController(vsync: this, duration: const Duration(milliseconds: 1200));

  @override
  void initState() {
    super.initState();
    _c.repeat(reverse: true);
  }

  @override
  void dispose() {
    _c.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return FadeTransition(
      opacity: Tween(begin: 0.35, end: 1.0).animate(_c),
      child: Container(
        width: 8,
        height: 8,
        decoration: const BoxDecoration(color: HexaColors.good, shape: BoxShape.circle),
      ),
    );
  }
}
