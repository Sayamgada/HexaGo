import 'package:flutter/material.dart';

import '../../../theme/hexago_theme.dart';
import '../../../util/format.dart';
import '../../../widgets/hexagon.dart';
import '../../money/money_widgets.dart';
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

/// Feed filter chips.
enum FeedFilter {
  all('All', null, 'No agent decisions yet'),
  matches('Matches', FeedCategory.match, 'No matches yet'),
  unmatched('Unmatched', FeedCategory.unmatched, 'No unmatched requests'),
  incidents('Incidents', FeedCategory.incident, 'No incidents yet'),
  money('Money', FeedCategory.money, 'No settlements yet');

  const FeedFilter(this.label, this.category, this.emptyText);
  final String label;
  final FeedCategory? category;
  final String emptyText;

  bool accepts(AgentEvent e) => category == null || e.category == category;
}

class AgentFeed extends StatefulWidget {
  const AgentFeed({super.key, required this.events, this.totalSeen});

  final List<AgentEvent> events;
  final int? totalSeen;

  @override
  State<AgentFeed> createState() => _AgentFeedState();
}

class _AgentFeedState extends State<AgentFeed> {
  FeedFilter _filter = FeedFilter.all;

  @override
  Widget build(BuildContext context) {
    final events = widget.events;
    final totalSeen = widget.totalSeen;
    final shown = _filter == FeedFilter.all ? events : events.where(_filter.accepts).toList();
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
          Padding(
            padding: const EdgeInsets.fromLTRB(12, 0, 12, 8),
            child: Wrap(spacing: 6, runSpacing: 6, children: [
              for (final f in FeedFilter.values)
                ChoiceChip(
                  key: ValueKey('feed-filter-${f.label}'),
                  label: Text(f.label, style: const TextStyle(fontSize: 11)),
                  selected: _filter == f,
                  showCheckmark: false,
                  visualDensity: VisualDensity.compact,
                  materialTapTargetSize: MaterialTapTargetSize.shrinkWrap,
                  onSelected: (_) => setState(() => _filter = f),
                ),
            ]),
          ),
          const Divider(height: 1),
          Expanded(
            child: events.isEmpty
                ? const Center(
                    child: Text('Waiting for agent decisions…', style: TextStyle(color: HexaColors.textMuted)),
                  )
                : shown.isEmpty
                    ? Center(child: Text(_filter.emptyText, style: const TextStyle(color: HexaColors.textMuted)))
                    : ListView.builder(
                        key: const ValueKey('agent-feed-list'),
                        padding: const EdgeInsets.symmetric(vertical: 4),
                        itemCount: shown.length,
                        itemBuilder: (context, i) => AgentEventTile(key: ValueKey(shown[i].id), event: shown[i]),
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
                    shortenIds(e.decision),
                    maxLines: _expanded ? null : 2,
                    overflow: _expanded ? null : TextOverflow.ellipsis,
                    style: const TextStyle(fontSize: 13, height: 1.25),
                  ),
                  if (e.rideId.isNotEmpty || e.driverId.isNotEmpty || e.tier.isNotEmpty || e.upgraded) ...[
                    const SizedBox(height: 4),
                    Wrap(spacing: 4, runSpacing: 4, children: [
                      if (e.rideId.isNotEmpty)
                        _Tag(shortId(e.rideId), HexaColors.textMuted, key: const ValueKey('ride-tag'), mono: true),
                      if (e.driverId.isNotEmpty)
                        _Tag(e.driverId, HexaColors.textMuted, key: const ValueKey('driver-tag'), mono: true),
                      if (e.tier.isNotEmpty) _Tag(e.tier.toUpperCase(), style.color, key: const ValueKey('tier-tag')),
                      if (e.upgraded) const _Tag('UPGRADED', HexaColors.good, key: ValueKey('upgraded-tag')),
                    ]),
                  ],
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
                      formatLatency(e.latencyMs),
                      key: const ValueKey('event-latency'),
                      style: const TextStyle(fontSize: 11, color: HexaColors.textMuted),
                    ),
                    const Spacer(),
                    Icon(_expanded ? Icons.expand_less : Icons.expand_more, size: 16, color: HexaColors.textMuted),
                  ]),
                  AnimatedSize(
                    duration: const Duration(milliseconds: 150),
                    alignment: Alignment.topCenter,
                    child: _expanded && (e.reasoning.isNotEmpty || e.rideId.isNotEmpty || e.driverId.isNotEmpty)
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
                            child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                              if (e.reasoning.isNotEmpty)
                                Text(
                                  e.reasoning,
                                  style: const TextStyle(fontSize: 12, color: Color(0xFFCBD5E1), height: 1.35),
                                ),
                              if (e.rideId.isNotEmpty || e.driverId.isNotEmpty)
                                Padding(
                                  key: const ValueKey('event-ids'),
                                  padding: EdgeInsets.only(top: e.reasoning.isNotEmpty ? 6 : 0),
                                  child: SelectableText(
                                    [
                                      if (e.rideId.isNotEmpty) 'Ride ${e.rideId}',
                                      if (e.driverId.isNotEmpty) 'Driver ${e.driverId}',
                                      if (e.upgraded && e.requestedTier.isNotEmpty)
                                        'Requested ${e.requestedTier}, served by ${e.tier}',
                                    ].join('\n'),
                                    style: const TextStyle(
                                        fontSize: 11, color: HexaColors.textMuted, fontFamily: 'monospace', height: 1.4),
                                  ),
                                ),
                              if (e.category == FeedCategory.money && e.rideId.isNotEmpty)
                                TextButton.icon(
                                  key: const ValueKey('fare-breakdown-button'),
                                  style: TextButton.styleFrom(
                                    padding: const EdgeInsets.symmetric(horizontal: 6),
                                    visualDensity: VisualDensity.compact,
                                  ),
                                  onPressed: () => showFareBreakdownSheet(context, e.rideId),
                                  icon: const Icon(Icons.receipt_long, size: 14),
                                  label: const Text('Fare breakdown', style: TextStyle(fontSize: 12)),
                                ),
                            ]),
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

/// Small outlined label (ride id, driver id, tier, UPGRADED).
class _Tag extends StatelessWidget {
  const _Tag(this.text, this.color, {super.key, this.mono = false});

  final String text;
  final Color color;
  final bool mono;

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 5, vertical: 1),
      decoration: BoxDecoration(
        color: color.withValues(alpha: 0.10),
        borderRadius: BorderRadius.circular(4),
        border: Border.all(color: color.withValues(alpha: 0.45)),
      ),
      child: Text(
        text,
        style: TextStyle(
          fontSize: 9.5,
          fontWeight: FontWeight.w700,
          color: color,
          letterSpacing: mono ? 0 : 0.5,
          fontFamily: mono ? 'monospace' : null,
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
