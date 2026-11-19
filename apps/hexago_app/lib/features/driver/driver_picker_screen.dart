import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../theme/hexago_theme.dart';
import '../../util/errors.dart';
import '../../widgets/waking_gate.dart';
import 'models/driver_models.dart';
import 'state/driver_providers.dart';
import 'widgets/driver_widgets.dart';

/// `/driver`: pick a simulated driver to take over.
class DriverPickerScreen extends StatelessWidget {
  const DriverPickerScreen({super.key, this.onPick, this.onBecomeDriver});

  /// Defaults to opening `/driver/<id>`.
  final void Function(BuildContext context, DriverSummary driver)? onPick;

  /// Defaults to opening `/driver/onboarding`.
  final void Function(BuildContext context)? onBecomeDriver;

  @override
  Widget build(BuildContext context) =>
      WakingGate(child: DriverPickerBody(onPick: onPick, onBecomeDriver: onBecomeDriver));
}

class DriverPickerBody extends ConsumerStatefulWidget {
  const DriverPickerBody({super.key, this.onPick, this.onBecomeDriver, this.refreshEvery = const Duration(seconds: 5)});

  final void Function(BuildContext context, DriverSummary driver)? onPick;
  final void Function(BuildContext context)? onBecomeDriver;
  final Duration refreshEvery;

  @override
  ConsumerState<DriverPickerBody> createState() => _DriverPickerBodyState();
}

class _DriverPickerBodyState extends ConsumerState<DriverPickerBody> {
  final _search = TextEditingController();
  String _tier = '';
  List<DriverSummary>? _all;
  Object? _error;
  Timer? _timer;

  @override
  void initState() {
    super.initState();
    unawaited(_load());
    _timer = Timer.periodic(widget.refreshEvery, (_) => _load());
    _search.addListener(() => setState(() {}));
  }

  @override
  void dispose() {
    _timer?.cancel();
    _search.dispose();
    super.dispose();
  }

  Future<void> _load() async {
    try {
      final list = await ref.read(driverApiProvider).drivers();
      if (mounted) {
        setState(() {
          _all = list;
          _error = null;
        });
      }
    } catch (e) {
      if (!mounted || WakingGate.report(context, e)) return;
      setState(() => _error = e);
    }
  }

  /// Client-side filtering keeps typing instant; the list refreshes every few seconds.
  List<DriverSummary> get _visible {
    final q = _search.text.trim().toLowerCase();
    return [
      for (final d in _all ?? const <DriverSummary>[])
        if ((_tier.isEmpty || d.tier == _tier) && (q.isEmpty || d.id.toLowerCase().contains(q))) d,
    ];
  }

  void _pick(DriverSummary d) {
    final cb = widget.onPick;
    if (cb != null) {
      cb(context, d);
    } else {
      context.push('/driver/${Uri.encodeComponent(d.id)}');
    }
  }

  @override
  Widget build(BuildContext context) {
    final visible = _visible;
    final all = _all;
    return Scaffold(
      appBar: AppBar(title: const Text('Driver: take over a car'), actions: [
        Padding(
          padding: const EdgeInsets.only(right: 8),
          child: FilledButton.tonalIcon(
            key: const ValueKey('become-driver'),
            onPressed: () {
              final cb = widget.onBecomeDriver;
              if (cb != null) {
                cb(context);
              } else {
                context.push('/driver/onboarding');
              }
            },
            icon: const Icon(Icons.person_add_alt_1, size: 18),
            label: const Text('Become a driver'),
          ),
        ),
      ]),
      body: Center(
        child: ConstrainedBox(
          constraints: const BoxConstraints(maxWidth: 900),
          child: Padding(
            padding: const EdgeInsets.all(16),
            child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
              TextField(
                key: const ValueKey('driver-search'),
                controller: _search,
                decoration: const InputDecoration(
                  prefixIcon: Icon(Icons.search),
                  hintText: 'Search by short id (e.g. 042)',
                  isDense: true,
                  border: OutlineInputBorder(),
                ),
              ),
              const SizedBox(height: 10),
              Wrap(spacing: 6, runSpacing: 6, children: [
                for (final t in ['', ...driverTiers])
                  ChoiceChip(
                    key: ValueKey('tier-${t.isEmpty ? 'all' : t}'),
                    label: Text(t.isEmpty ? 'All' : tierLabel(t)),
                    selected: _tier == t,
                    onSelected: (_) => setState(() => _tier = t),
                  ),
              ]),
              const SizedBox(height: 8),
              if (all != null)
                Text(
                  '${visible.length} of ${all.length} drivers · '
                  '${all.where((d) => d.phase == DriverPhase.onTrip).length} on a trip',
                  key: const ValueKey('driver-count'),
                  style: const TextStyle(color: HexaColors.textMuted, fontSize: 12),
                ),
              const SizedBox(height: 8),
              Expanded(child: _list(visible)),
            ]),
          ),
        ),
      ),
    );
  }

  Widget _list(List<DriverSummary> visible) {
    if (_all == null) {
      if (_error != null) {
        return Center(
          child: Column(mainAxisSize: MainAxisSize.min, children: [
            Text(friendlyError(_error, action: 'load the drivers'), style: const TextStyle(color: HexaColors.bad)),
            const SizedBox(height: 8),
            OutlinedButton(onPressed: _load, child: const Text('Retry')),
          ]),
        );
      }
      return const Center(child: CircularProgressIndicator());
    }
    if (visible.isEmpty) {
      return const Center(
        key: ValueKey('driver-empty'),
        child: Text('No drivers match.', style: TextStyle(color: HexaColors.textMuted)),
      );
    }
    return ListView.separated(
      itemCount: visible.length,
      separatorBuilder: (_, _) => const SizedBox(height: 6),
      itemBuilder: (context, i) {
        final d = visible[i];
        return Card(
          key: ValueKey('driver-row-${d.id}'),
          margin: EdgeInsets.zero,
          child: ListTile(
            onTap: () => _pick(d),
            leading: CircleAvatar(
              backgroundColor: phaseColor(d.phase).withValues(alpha: 0.18),
              child: Icon(d.av ? Icons.smart_toy_outlined : Icons.local_taxi, color: phaseColor(d.phase)),
            ),
            title: Row(children: [
              Text(d.id, style: const TextStyle(fontFamily: 'monospace', fontWeight: FontWeight.w700)),
              const SizedBox(width: 8),
              Text(tierLabel(d.tier), style: const TextStyle(color: HexaColors.textMuted, fontSize: 12)),
              if (d.av) ...[const SizedBox(width: 8), AvBadge(key: ValueKey('av-badge-${d.id}'))],
            ]),
            subtitle: Text(
              [
                if (d.rating > 0) '★ ${d.rating.toStringAsFixed(1)}',
                if (d.batteryPct != null) '${d.batteryPct!.round()}% battery',
                if (d.suspended && d.incidentId.isNotEmpty) 'incident ${d.incidentId}',
              ].join(' · '),
              style: const TextStyle(fontSize: 12),
            ),
            trailing: Row(mainAxisSize: MainAxisSize.min, children: [
              StatusChip(phase: d.phase),
              const SizedBox(width: 6),
              const Icon(Icons.chevron_right),
            ]),
          ),
        );
      },
    );
  }
}
