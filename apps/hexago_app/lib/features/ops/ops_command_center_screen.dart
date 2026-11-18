import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../theme/hexago_theme.dart';
import '../safety/widgets/incident_panel.dart';
import 'models/ops_models.dart';
import 'state/ops_providers.dart';
import 'widgets/agent_feed.dart';
import 'widgets/fleet_map.dart';
import 'widgets/metrics_strip.dart';
import 'widgets/ops_banner.dart';
import 'widgets/scenario_bar.dart';

/// "Zero humans in the loop" showcase: live fleet map, dispatch KPIs,
/// streaming agent decisions and what-if scenario injection.
class OpsCommandCenterScreen extends ConsumerWidget {
  const OpsCommandCenterScreen({super.key});

  static const wideBreakpoint = 1100.0;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    return Scaffold(
      key: const ValueKey('ops-command-center'),
      backgroundColor: HexaColors.background,
      body: SafeArea(
        child: LayoutBuilder(builder: (context, c) {
          final wide = c.maxWidth >= wideBreakpoint;
          const pad = EdgeInsets.all(12);
          if (wide) {
            return Padding(
              padding: pad,
              child: Column(children: [
                const _Banner(),
                const SizedBox(height: 10),
                const _Metrics(),
                const SizedBox(height: 10),
                Expanded(
                  child: Row(crossAxisAlignment: CrossAxisAlignment.stretch, children: const [
                    Expanded(child: _MapPanel(overlayScenarios: true)),
                    SizedBox(width: 10),
                    SizedBox(width: 420, child: _SidePanel()),
                  ]),
                ),
              ]),
            );
          }
          // Narrow / mobile: stacked and scrollable.
          return ListView(
            padding: pad,
            children: [
              const _Banner(),
              const SizedBox(height: 10),
              const _Metrics(),
              const SizedBox(height: 10),
              const _Scenarios(),
              const SizedBox(height: 10),
              SizedBox(height: (c.maxHeight * 0.55).clamp(320.0, 560.0), child: const _MapPanel(overlayScenarios: false)),
              const SizedBox(height: 10),
              const SizedBox(height: 520, child: _Feed()),
              const SizedBox(height: 10),
              const SizedBox(height: 520, child: IncidentPanel()),
            ],
          );
        }),
      ),
    );
  }
}

class _Banner extends ConsumerWidget {
  const _Banner();

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final source = ref.watch(opsSourceProvider);
    return ZeroHumansBanner(
      decisionsToday: ref.watch(decisionsTodayProvider),
      status: ref.watch(connectionStatusProvider),
      modeLabel: source.modeLabel,
      onBack: () => context.canPop() ? context.pop() : context.go('/'),
    );
  }
}

class _Metrics extends ConsumerWidget {
  const _Metrics();

  @override
  Widget build(BuildContext context, WidgetRef ref) => MetricsStrip(metrics: ref.watch(metricsProvider));
}

class _Feed extends ConsumerWidget {
  const _Feed();

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final feed = ref.watch(agentFeedProvider);
    return AgentFeed(events: feed.events, totalSeen: feed.totalSeen);
  }
}

/// Wide layout's right column: the agent feed and the safety incident panel.
class _SidePanel extends StatelessWidget {
  const _SidePanel();

  @override
  Widget build(BuildContext context) {
    return DefaultTabController(
      length: 2,
      child: Column(children: [
        const TabBar(
          key: ValueKey('ops-side-tabs'),
          tabs: [
            Tab(key: ValueKey('tab-feed'), height: 36, text: 'Agent feed'),
            Tab(key: ValueKey('tab-safety'), height: 36, text: 'Safety'),
          ],
        ),
        const SizedBox(height: 8),
        const Expanded(child: TabBarView(physics: NeverScrollableScrollPhysics(), children: [_Feed(), IncidentPanel()])),
      ]),
    );
  }
}

class _Scenarios extends ConsumerWidget {
  const _Scenarios();

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    return ScenarioBar(onTrigger: (s) async {
      final messenger = ScaffoldMessenger.of(context);
      try {
        await ref.read(opsSourceProvider).triggerScenario(s);
        messenger.showSnackBar(SnackBar(
          content: Text('${s.label} injected. Agents are responding autonomously.'),
          duration: const Duration(seconds: 2),
        ));
      } catch (e) {
        messenger.showSnackBar(SnackBar(
          backgroundColor: HexaColors.bad,
          content: Text('Could not start ${s.label}: $e'),
        ));
      }
    });
  }
}

class _MapPanel extends ConsumerWidget {
  const _MapPanel({required this.overlayScenarios});

  final bool overlayScenarios;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final status = ref.watch(connectionStatusProvider);
    // Show the cold-start overlay until the first fleet snapshot arrives.
    final tracker = ref.watch(fleetTrackerProvider);
    return Container(
      decoration: BoxDecoration(
        borderRadius: BorderRadius.circular(12),
        border: Border.all(color: HexaColors.border),
      ),
      child: Stack(children: [
        const Positioned.fill(child: FleetMap()),
        if (overlayScenarios) const Positioned(left: 10, top: 10, right: 10, child: _Scenarios()),
        Positioned.fill(
          child: ListenableBuilder(
            listenable: tracker,
            builder: (context, _) => status != OpsConnectionStatus.live && tracker.isEmpty
                ? ClipRRect(
                    borderRadius: BorderRadius.circular(12),
                    child: WakingOverlay(reconnecting: status == OpsConnectionStatus.reconnecting),
                  )
                : const SizedBox.shrink(),
          ),
        ),
      ]),
    );
  }
}
