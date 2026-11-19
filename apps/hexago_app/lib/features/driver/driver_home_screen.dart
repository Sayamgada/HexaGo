import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../theme/hexago_theme.dart';
import '../../util/errors.dart';
import '../../widgets/waking_gate.dart';
import '../ops/models/ops_models.dart';
import 'models/driver_models.dart';
import 'state/driver_providers.dart';
import 'widgets/driver_earnings.dart';
import 'widgets/driver_map.dart';
import 'widgets/driver_widgets.dart';

/// `/driver/:id`: the taken-over driver's home (live map, status, ride, AV
/// panel, Safety freeze) and earnings.
class DriverHomeScreen extends StatelessWidget {
  const DriverHomeScreen({super.key, required this.driverId, this.showLiveMap = true});

  final String driverId;
  final bool showLiveMap;

  @override
  Widget build(BuildContext context) => WakingGate(child: DriverHomeBody(driverId: driverId, showLiveMap: showLiveMap));
}

class DriverHomeBody extends ConsumerStatefulWidget {
  const DriverHomeBody({super.key, required this.driverId, this.showLiveMap = true});

  final String driverId;
  final bool showLiveMap;

  @override
  ConsumerState<DriverHomeBody> createState() => _DriverHomeBodyState();
}

class _DriverHomeBodyState extends ConsumerState<DriverHomeBody> {
  DriverDetail? _detail;
  DriverPosition? _live; // newest position from the drivers stream
  Object? _error;
  Timer? _timer;
  StreamSubscription<List<DriverPosition>>? _sub;

  @override
  void initState() {
    super.initState();
    unawaited(_load());
    _timer = Timer.periodic(ref.read(driverPollProvider), (_) => _load());
    if (widget.showLiveMap) {
      _sub = ref.read(driverPositionsProvider).listen((batch) {
        for (final p in batch) {
          if (p.id == widget.driverId) {
            if (mounted) setState(() => _live = p);
            break;
          }
        }
      });
    }
  }

  @override
  void dispose() {
    _timer?.cancel();
    _sub?.cancel();
    super.dispose();
  }

  Future<void> _load() async {
    try {
      final d = await ref.read(driverApiProvider).driver(widget.driverId);
      if (mounted) {
        setState(() {
          _detail = d;
          _error = null;
        });
      }
    } catch (e) {
      if (!mounted || WakingGate.report(context, e)) return;
      setState(() => _error = e);
    }
  }

  @override
  Widget build(BuildContext context) {
    final d = _detail;
    return DefaultTabController(
      length: 2,
      child: Scaffold(
        appBar: AppBar(
          title: Row(children: [
            Text(widget.driverId, style: const TextStyle(fontFamily: 'monospace', fontWeight: FontWeight.w800)),
            if (d != null) ...[
              const SizedBox(width: 8),
              Text(tierLabel(d.summary.tier), style: const TextStyle(color: HexaColors.textMuted, fontSize: 14)),
              if (d.summary.av) ...[const SizedBox(width: 8), const AvBadge()],
              const SizedBox(width: 10),
              StatusChip(phase: d.phase),
            ],
          ]),
          bottom: const TabBar(tabs: [
            Tab(key: ValueKey('tab-drive'), icon: Icon(Icons.map_outlined, size: 18), text: 'Drive'),
            Tab(key: ValueKey('tab-earnings'), icon: Icon(Icons.payments_outlined, size: 18), text: 'Earnings'),
          ]),
        ),
        body: TabBarView(physics: const NeverScrollableScrollPhysics(), children: [
          _drive(d),
          DriverEarningsView(driverId: widget.driverId),
        ]),
      ),
    );
  }

  Widget _drive(DriverDetail? d) {
    if (d == null) {
      return Center(
        child: _error == null
            ? const CircularProgressIndicator()
            : Text(friendlyError(_error, action: 'load this driver'), style: const TextStyle(color: HexaColors.bad)),
      );
    }
    // The stream is fresher than the poll, but a suspended car is frozen in place.
    final live = d.suspended ? null : _live;
    final side = <Widget>[
      if (d.suspended) ...[
        FrozenBanner(incidentId: d.incidentId, reason: d.suspensionReason, since: d.suspendedAt),
        const SizedBox(height: 10),
      ],
      RideCard(ride: d.ride),
      if (d.avStatus != null) ...[const SizedBox(height: 10), AvPanel(status: d.avStatus!)],
      const SizedBox(height: 10),
      Text(
        '${d.speedKmh.round()} km/h · ★ ${d.summary.rating.toStringAsFixed(1)}',
        style: const TextStyle(color: HexaColors.textMuted, fontSize: 12),
      ),
    ];
    final map = widget.showLiveMap
        ? DriverMap(
            lat: live?.lat ?? d.summary.lat,
            lng: live?.lng ?? d.summary.lng,
            heading: live?.heading ?? d.heading,
            phase: d.phase,
            ride: d.ride,
            av: d.summary.av,
          )
        : const SizedBox.shrink();
    return LayoutBuilder(builder: (context, c) {
      if (c.maxWidth > 860) {
        return Padding(
          padding: const EdgeInsets.all(16),
          child: Row(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
            Expanded(flex: 3, child: map),
            const SizedBox(width: 16),
            SizedBox(width: 360, child: ListView(children: side)),
          ]),
        );
      }
      return ListView(padding: const EdgeInsets.all(12), children: [
        if (widget.showLiveMap) SizedBox(height: 320, child: map),
        const SizedBox(height: 12),
        ...side,
      ]);
    });
  }
}
