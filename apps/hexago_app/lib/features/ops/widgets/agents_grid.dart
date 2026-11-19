import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../theme/hexago_theme.dart';
import '../../../util/errors.dart';
import '../../../util/format.dart';
import '../../../widgets/hexagon.dart';
import '../../platform/models/platform_models.dart';
import '../../platform/state/platform_providers.dart';
import 'agent_feed.dart' show AgentStyle;

/// Agents tab: the 11-agent roster from `GET /v1/agents`, refreshed every 10 s.
class AgentsGrid extends ConsumerWidget {
  const AgentsGrid({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final roster = ref.watch(agentRosterProvider);
    return Container(
      key: const ValueKey('agents-grid'),
      decoration: BoxDecoration(
        color: HexaColors.surface,
        borderRadius: BorderRadius.circular(12),
        border: Border.all(color: HexaColors.border),
      ),
      child: roster.when(
        loading: () => const Center(child: CircularProgressIndicator()),
        error: (e, _) => Center(
          child: Padding(
            padding: const EdgeInsets.all(20),
            child: Column(
              mainAxisSize: MainAxisSize.min,
              children: [
                Text(
                  isWakeError(e) ? 'Waking up the agent swarm...' : friendlyError(e, action: 'load the agents'),
                  key: const ValueKey('agents-error'),
                  textAlign: TextAlign.center,
                  style: const TextStyle(color: HexaColors.textMuted),
                ),
                const SizedBox(height: 10),
                TextButton.icon(
                  onPressed: () => ref.read(agentRosterProvider.notifier).refreshNow(),
                  icon: const Icon(Icons.refresh, size: 16),
                  label: const Text('Retry'),
                ),
              ],
            ),
          ),
        ),
        data: (agents) => _Grid(agents: agents),
      ),
    );
  }
}

class _Grid extends StatelessWidget {
  const _Grid({required this.agents});
  final List<AgentInfo> agents;

  @override
  Widget build(BuildContext context) {
    final active = agents.where((a) => a.active).length;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        Padding(
          padding: const EdgeInsets.fromLTRB(14, 12, 14, 8),
          child: Row(
            children: [
              const Text('Agent swarm', style: TextStyle(fontWeight: FontWeight.w700, fontSize: 15)),
              const Spacer(),
              Text(
                '$active of ${agents.length} active',
                key: const ValueKey('agents-active-count'),
                style: const TextStyle(fontSize: 11, color: HexaColors.textMuted),
              ),
            ],
          ),
        ),
        const Divider(height: 1),
        Expanded(
          child: LayoutBuilder(
            builder: (context, c) {
              final cols = c.maxWidth >= 560 ? 3 : (c.maxWidth >= 340 ? 2 : 1);
              return GridView.builder(
                padding: const EdgeInsets.all(10),
                gridDelegate: SliverGridDelegateWithFixedCrossAxisCount(
                  crossAxisCount: cols,
                  mainAxisSpacing: 8,
                  crossAxisSpacing: 8,
                  mainAxisExtent: 168,
                ),
                itemCount: agents.length,
                itemBuilder: (context, i) => AgentCard(agent: agents[i]),
              );
            },
          ),
        ),
      ],
    );
  }
}

/// One agent: icon, name, active/planned, decisions in the last hour,
/// fallback rate, last action and last model. Planned agents are greyed.
class AgentCard extends StatelessWidget {
  const AgentCard({super.key, required this.agent});
  final AgentInfo agent;

  @override
  Widget build(BuildContext context) {
    final a = agent;
    final style = AgentStyle.of(a.id);
    final color = a.active ? style.color : HexaColors.offline;
    final fbColor = a.fallbackRate >= 0.5
        ? HexaColors.bad
        : a.fallbackRate >= 0.2
        ? HexaColors.warn
        : HexaColors.good;
    final card = Container(
      key: ValueKey('agent-card-${a.id}'),
      padding: const EdgeInsets.all(10),
      decoration: BoxDecoration(
        color: HexaColors.panel,
        borderRadius: BorderRadius.circular(10),
        border: Border.all(color: a.active ? color.withValues(alpha: 0.45) : HexaColors.border),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Row(
            children: [
              HexBadge(
                size: 28,
                color: color,
                child: Icon(style.icon, size: 14, color: color),
              ),
              const SizedBox(width: 8),
              Expanded(
                child: Text(
                  a.name,
                  maxLines: 1,
                  overflow: TextOverflow.ellipsis,
                  style: TextStyle(
                    fontWeight: FontWeight.w700,
                    fontSize: 12.5,
                    color: a.active ? Colors.white : HexaColors.textMuted,
                  ),
                ),
              ),
              _StatusTag(active: a.active),
            ],
          ),
          const SizedBox(height: 8),
          if (a.active) ...[
            Wrap(
              spacing: 12,
              runSpacing: 2,
              children: [
                _Stat(id: 'decisions', label: 'last hour', value: formatCount(a.decisionsLastHour)),
                _Stat(
                  id: 'fallback',
                  label: 'fallback',
                  value: '${(a.fallbackRate * 100).round()}%',
                  color: a.decisionsLastHour == 0 ? null : fbColor,
                ),
              ],
            ),
            const SizedBox(height: 6),
            Expanded(
              child: Text(
                a.lastDecision.isEmpty ? 'No decisions yet' : shortenIds(a.lastDecision),
                key: const ValueKey('agent-last-action'),
                maxLines: 2,
                overflow: TextOverflow.ellipsis,
                style: const TextStyle(fontSize: 11.5, height: 1.3, color: Color(0xFFCBD5E1)),
              ),
            ),
            Row(
              children: [
                const Icon(Icons.memory, size: 11, color: HexaColors.textMuted),
                const SizedBox(width: 3),
                Expanded(
                  child: Text(
                    modelLabel(a.lastModel),
                    key: const ValueKey('agent-last-model'),
                    maxLines: 1,
                    overflow: TextOverflow.ellipsis,
                    style: const TextStyle(fontSize: 10.5, color: HexaColors.textMuted),
                  ),
                ),
                if (a.lastAt != null)
                  Text(formatClock(a.lastAt!), style: const TextStyle(fontSize: 10, color: HexaColors.textMuted)),
              ],
            ),
          ] else
            Expanded(
              child: Text(
                a.responsibility.isEmpty ? 'Planned: not running yet.' : a.responsibility,
                maxLines: 4,
                overflow: TextOverflow.ellipsis,
                style: const TextStyle(fontSize: 11, height: 1.3, color: HexaColors.textMuted),
              ),
            ),
        ],
      ),
    );
    return a.active ? card : Opacity(opacity: 0.55, child: card);
  }
}

/// "gemini/gemini-3.5-flash-lite" -> "gemini-3.5-flash-lite"; empty -> rule engine.
String modelLabel(String model) {
  if (model.isEmpty) return 'Deterministic rules';
  final i = model.lastIndexOf('/');
  return i >= 0 && i < model.length - 1 ? model.substring(i + 1) : model;
}

class _StatusTag extends StatelessWidget {
  const _StatusTag({required this.active});
  final bool active;

  @override
  Widget build(BuildContext context) {
    final c = active ? HexaColors.good : HexaColors.offline;
    return Container(
      key: ValueKey(active ? 'agent-active' : 'agent-planned'),
      padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 2),
      decoration: BoxDecoration(
        color: c.withValues(alpha: 0.14),
        borderRadius: BorderRadius.circular(4),
        border: Border.all(color: c.withValues(alpha: 0.6)),
      ),
      child: Text(
        active ? 'ACTIVE' : 'PLANNED',
        style: TextStyle(fontSize: 9, fontWeight: FontWeight.w800, letterSpacing: 0.6, color: c),
      ),
    );
  }
}

class _Stat extends StatelessWidget {
  const _Stat({required this.id, required this.label, required this.value, this.color});
  final String id;
  final String label;
  final String value;
  final Color? color;

  @override
  Widget build(BuildContext context) {
    return Row(
      mainAxisSize: MainAxisSize.min,
      crossAxisAlignment: CrossAxisAlignment.baseline,
      textBaseline: TextBaseline.alphabetic,
      children: [
        Text(
          value,
          key: ValueKey('agent-stat-$id'),
          style: TextStyle(fontSize: 15, fontWeight: FontWeight.w700, color: color ?? Colors.white),
        ),
        const SizedBox(width: 3),
        Text(label, style: const TextStyle(fontSize: 10, color: HexaColors.textMuted)),
      ],
    );
  }
}
