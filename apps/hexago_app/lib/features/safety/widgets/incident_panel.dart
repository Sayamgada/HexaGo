import 'dart:async';
import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:latlong2/latlong.dart' show LatLng;

import '../../../theme/hexago_theme.dart';
import '../../../util/errors.dart';
import '../../../util/format.dart';
import '../../ops/state/ops_providers.dart';
import '../data/safety_api.dart';
import '../models/safety_models.dart';
import '../safety_call_screen.dart';
import '../state/safety_providers.dart';

Color severityColor(double s) => s >= 0.8 ? HexaColors.bad : (s >= 0.3 ? HexaColors.warn : HexaColors.good);

Color statusColorFor(String status) => switch (status) {
      'escalated' => HexaColors.bad,
      'check_in' => HexaColors.warn,
      'monitoring' => HexaColors.cyan,
      'resolved' => HexaColors.good,
      _ => HexaColors.textMuted,
    };

String statusLabel(String status) => switch (status) {
      'check_in' => 'CHECKING IN',
      '' => 'UNKNOWN',
      _ => status.toUpperCase(),
    };

/// Ops console: live rider check-ins ("Open rider phone") and the incident list.
class IncidentPanel extends ConsumerStatefulWidget {
  const IncidentPanel({super.key, this.pollEvery = const Duration(seconds: 3)});

  final Duration pollEvery;

  @override
  ConsumerState<IncidentPanel> createState() => _IncidentPanelState();
}

class _IncidentPanelState extends ConsumerState<IncidentPanel> {
  late final SafetyApi _api = ref.read(safetyApiProvider);
  IncidentList? _list;
  String? _error;
  Timer? _timer;
  bool _loading = false;

  @override
  void initState() {
    super.initState();
    unawaited(_refresh());
    _timer = Timer.periodic(widget.pollEvery, (_) => _refresh());
  }

  @override
  void dispose() {
    _timer?.cancel();
    super.dispose();
  }

  Future<void> _refresh() async {
    if (_loading) return;
    _loading = true;
    try {
      final l = await _api.listIncidents();
      if (mounted) setState(() => (_list = l, _error = null));
    } catch (e) {
      if (mounted) setState(() => _error = 'Safety service unavailable');
    } finally {
      _loading = false;
    }
  }

  Future<void> _openPhone(String checkinId) async {
    await showRiderPhone(context, checkinId);
    unawaited(_refresh());
  }

  Future<void> _openIncident(String id) => showDialog<void>(
        context: context,
        builder: (ctx) => Dialog(
          insetPadding: const EdgeInsets.all(16),
          child: SizedBox(
            width: 760,
            height: math.min(820, MediaQuery.sizeOf(ctx).height - 40),
            child: IncidentDetailLoader(incidentId: id, onClose: () => Navigator.of(ctx).pop()),
          ),
        ),
      );

  @override
  Widget build(BuildContext context) {
    final list = _list;
    return Container(
      key: const ValueKey('incident-panel'),
      decoration: BoxDecoration(
        color: HexaColors.panel,
        borderRadius: BorderRadius.circular(12),
        border: Border.all(color: HexaColors.border),
      ),
      child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
        Padding(
          padding: const EdgeInsets.fromLTRB(12, 10, 6, 6),
          child: Row(children: [
            const Icon(Icons.health_and_safety_outlined, size: 18, color: HexaColors.incident),
            const SizedBox(width: 8),
            const Expanded(child: Text('Safety incidents', style: TextStyle(fontWeight: FontWeight.w600))),
            if (list != null && list.suppressed > 0)
              Tooltip(
                message: 'Escalations suppressed by the rate limit (likely false positives)',
                child: Text('${list.suppressed} suppressed', style: const TextStyle(color: HexaColors.textMuted, fontSize: 11)),
              ),
            IconButton(tooltip: 'Refresh', onPressed: _refresh, icon: const Icon(Icons.refresh, size: 18)),
          ]),
        ),
        if (_error != null)
          Padding(
            padding: const EdgeInsets.symmetric(horizontal: 12),
            child: Text(_error!, style: const TextStyle(color: HexaColors.bad, fontSize: 12)),
          ),
        if (list != null)
          for (final c in list.pendingCalls) _LiveCallTile(call: c, onOpen: () => _openPhone(c.checkinId)),
        Expanded(
          child: list == null
              ? const Center(child: CircularProgressIndicator(strokeWidth: 2))
              : list.incidents.isEmpty
                  ? const Center(
                      child: Text('No incidents. Trigger "Safety incident" to test the response.',
                          textAlign: TextAlign.center, style: TextStyle(color: HexaColors.textMuted)))
                  : ListView.separated(
                      padding: const EdgeInsets.fromLTRB(8, 4, 8, 8),
                      itemCount: list.incidents.length,
                      separatorBuilder: (_, _) => const SizedBox(height: 6),
                      itemBuilder: (_, i) => IncidentTile(incident: list.incidents[i], onTap: () => _openIncident(list.incidents[i].id)),
                    ),
        ),
      ]),
    );
  }
}

class _LiveCallTile extends StatelessWidget {
  const _LiveCallTile({required this.call, required this.onOpen});
  final SafetyCall call;
  final VoidCallback onOpen;

  @override
  Widget build(BuildContext context) {
    return Container(
      key: ValueKey('live-checkin-${call.checkinId}'),
      margin: const EdgeInsets.fromLTRB(8, 2, 8, 6),
      padding: const EdgeInsets.fromLTRB(10, 8, 8, 8),
      decoration: BoxDecoration(
        color: HexaColors.warn.withValues(alpha: 0.12),
        borderRadius: BorderRadius.circular(10),
        border: Border.all(color: HexaColors.warn),
      ),
      child: Row(children: [
        const Icon(Icons.phone_in_talk, color: HexaColors.warn, size: 20),
        const SizedBox(width: 8),
        Expanded(
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Text('Live check-in · ride ${shortId(call.rideId)}', style: const TextStyle(fontWeight: FontWeight.w600, fontSize: 13)),
            Text('${call.driverId} · ${call.remainingS.ceil()} s left before escalation',
                style: const TextStyle(color: HexaColors.textMuted, fontSize: 11.5)),
          ]),
        ),
        FilledButton.icon(
          key: ValueKey('open-rider-phone-${call.checkinId}'),
          onPressed: onOpen,
          style: FilledButton.styleFrom(backgroundColor: HexaColors.warn, foregroundColor: Colors.black,
              visualDensity: VisualDensity.compact),
          icon: const Icon(Icons.smartphone, size: 16),
          label: const Text('Open rider phone', style: TextStyle(fontSize: 12)),
        ),
      ]),
    );
  }
}

class IncidentTile extends StatelessWidget {
  const IncidentTile({super.key, required this.incident, this.onTap});
  final IncidentSummary incident;
  final VoidCallback? onTap;

  @override
  Widget build(BuildContext context) {
    final i = incident;
    return Material(
      color: HexaColors.surface,
      borderRadius: BorderRadius.circular(10),
      child: InkWell(
        key: ValueKey('incident-${i.id}'),
        borderRadius: BorderRadius.circular(10),
        onTap: onTap,
        child: Padding(
          padding: const EdgeInsets.all(10),
          child: Row(children: [
            _SeverityBadge(severity: i.severity),
            const SizedBox(width: 10),
            Expanded(
              child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                Row(children: [
                  Flexible(
                    child: Text('${i.driverId} · ride ${shortId(i.rideId)}',
                        overflow: TextOverflow.ellipsis, style: const TextStyle(fontWeight: FontWeight.w600, fontSize: 13)),
                  ),
                  const SizedBox(width: 6),
                  _Chip(statusLabel(i.status), statusColorFor(i.status)),
                ]),
                const SizedBox(height: 3),
                Text(i.reason, maxLines: 2, overflow: TextOverflow.ellipsis,
                    style: const TextStyle(color: HexaColors.textMuted, fontSize: 11.5)),
              ]),
            ),
            if (i.createdAt != null)
              Text(formatClock(i.createdAt!), style: const TextStyle(color: HexaColors.textMuted, fontSize: 11)),
          ]),
        ),
      ),
    );
  }
}

class _SeverityBadge extends StatelessWidget {
  const _SeverityBadge({required this.severity});
  final double severity;

  @override
  Widget build(BuildContext context) {
    final c = severityColor(severity);
    return Container(
      width: 44,
      padding: const EdgeInsets.symmetric(vertical: 6),
      decoration: BoxDecoration(color: c.withValues(alpha: 0.15), borderRadius: BorderRadius.circular(8), border: Border.all(color: c)),
      child: Column(children: [
        Text(severity.toStringAsFixed(2), style: TextStyle(color: c, fontWeight: FontWeight.w700, fontSize: 13)),
        const Text('sev', style: TextStyle(color: HexaColors.textMuted, fontSize: 9)),
      ]),
    );
  }
}

class _Chip extends StatelessWidget {
  const _Chip(this.label, this.color);
  final String label;
  final Color color;

  @override
  Widget build(BuildContext context) => Container(
        padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 2),
        decoration: BoxDecoration(color: color.withValues(alpha: 0.15), borderRadius: BorderRadius.circular(6)),
        child: Text(label, style: TextStyle(color: color, fontSize: 10, fontWeight: FontWeight.w700, letterSpacing: 0.4)),
      );
}

/// Loads an incident and renders [IncidentDetailView]; refreshes while it is live.
class IncidentDetailLoader extends ConsumerStatefulWidget {
  const IncidentDetailLoader({super.key, required this.incidentId, this.onClose, this.pollEvery = const Duration(seconds: 3)});
  final String incidentId;
  final VoidCallback? onClose;
  final Duration pollEvery;

  @override
  ConsumerState<IncidentDetailLoader> createState() => _IncidentDetailLoaderState();
}

class _IncidentDetailLoaderState extends ConsumerState<IncidentDetailLoader> {
  late final SafetyApi _api = ref.read(safetyApiProvider);
  IncidentDetail? _detail;
  String? _error;
  Timer? _timer;

  @override
  void initState() {
    super.initState();
    unawaited(_load());
    _timer = Timer.periodic(widget.pollEvery, (_) {
      if (_detail?.summary.status != 'resolved') unawaited(_load());
    });
  }

  @override
  void dispose() {
    _timer?.cancel();
    super.dispose();
  }

  Future<void> _load() async {
    try {
      final d = await _api.getIncident(widget.incidentId);
      if (mounted) setState(() => (_detail = d, _error = null));
    } catch (e) {
      if (mounted) setState(() => _error = friendlyError(e, action: 'load the incident'));
    }
  }

  @override
  Widget build(BuildContext context) {
    final d = _detail;
    if (d == null) {
      return Center(child: _error == null ? const CircularProgressIndicator() : Text(_error!));
    }
    return IncidentDetailView(detail: d, onClose: widget.onClose);
  }
}

class IncidentDetailView extends StatelessWidget {
  const IncidentDetailView({super.key, required this.detail, this.onClose});
  final IncidentDetail detail;
  final VoidCallback? onClose;

  @override
  Widget build(BuildContext context) {
    final s = detail.summary;
    final f = s.flags;
    return Scaffold(
      key: const ValueKey('incident-detail'),
      backgroundColor: HexaColors.panel,
      appBar: AppBar(
        backgroundColor: HexaColors.panel,
        automaticallyImplyLeading: false,
        title: Row(children: [
          _SeverityBadge(severity: s.severity),
          const SizedBox(width: 10),
          Expanded(child: Text('Incident ${shortId(s.id.replaceFirst('inc_', ''))} · ${s.driverId}', overflow: TextOverflow.ellipsis)),
          _Chip(statusLabel(s.status), statusColorFor(s.status)),
        ]),
        actions: [if (onClose != null) IconButton(onPressed: onClose, icon: const Icon(Icons.close))],
      ),
      body: ListView(padding: const EdgeInsets.all(14), children: [
        Wrap(spacing: 8, runSpacing: 8, children: [
          ActionChipView(label: 'Driver frozen', done: f.driverFrozen, icon: Icons.block),
          ActionChipView(label: 'Emergency notified', done: f.emergencyNotified, icon: Icons.local_police_outlined,
              detail: detail.emergencyReference),
          ActionChipView(label: 'Location shared', done: f.locationShared, icon: Icons.share_location),
          ActionChipView(label: 'Contact notified', done: f.contactNotified, icon: Icons.sms_outlined),
          if (detail.replacementRideId != null)
            ActionChipView(label: 'Replacement AV dispatched', done: true, icon: Icons.electric_car,
                detail: shortId(detail.replacementRideId!)),
        ]),
        if (detail.trackUrl != null) ...[
          const SizedBox(height: 6),
          SelectableText('Live location: ${detail.trackUrl}', style: const TextStyle(color: HexaColors.cyan, fontSize: 12)),
        ],
        const SizedBox(height: 14),
        SizedBox(height: 280, child: PathReplay(plan: detail.plannedRoute, path: detail.vehiclePath, deviationM: detail.deviationM)),
        const SizedBox(height: 14),
        _Section('Reasoning${detail.reasoningSource.isEmpty ? '' : ' (${detail.reasoningSource})'}'),
        Text(detail.reasoning.isEmpty ? s.reason : detail.reasoning, style: const TextStyle(fontSize: 13, height: 1.4)),
        const SizedBox(height: 14),
        const _Section('Transcript'),
        if (detail.transcript.isEmpty)
          const Text('No call was placed.', style: TextStyle(color: HexaColors.textMuted, fontSize: 12))
        else
          for (final t in detail.transcript) TranscriptBubble(line: t),
        const SizedBox(height: 14),
        const _Section('Timeline'),
        IncidentTimeline(entries: detail.timeline),
        if (detail.resolution != null) ...[
          const SizedBox(height: 10),
          Text('Resolution: ${detail.resolution}', style: const TextStyle(color: HexaColors.good)),
        ],
      ]),
    );
  }
}

class _Section extends StatelessWidget {
  const _Section(this.title);
  final String title;

  @override
  Widget build(BuildContext context) => Padding(
        padding: const EdgeInsets.only(bottom: 6),
        child: Text(title.toUpperCase(),
            style: const TextStyle(color: HexaColors.textMuted, fontSize: 11, fontWeight: FontWeight.w700, letterSpacing: 0.8)),
      );
}

class ActionChipView extends StatelessWidget {
  const ActionChipView({super.key, required this.label, required this.done, required this.icon, this.detail});
  final String label;
  final bool done;
  final IconData icon;
  final String? detail;

  @override
  Widget build(BuildContext context) {
    final c = done ? HexaColors.good : HexaColors.textMuted;
    return Container(
      key: ValueKey('action-$label'),
      padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 6),
      decoration: BoxDecoration(
        color: c.withValues(alpha: done ? 0.14 : 0.06),
        borderRadius: BorderRadius.circular(20),
        border: Border.all(color: c.withValues(alpha: 0.6)),
      ),
      child: Row(mainAxisSize: MainAxisSize.min, children: [
        Icon(done ? Icons.check_circle : icon, size: 15, color: c),
        const SizedBox(width: 6),
        Text(detail == null ? label : '$label · $detail', style: TextStyle(color: c, fontSize: 12)),
      ]),
    );
  }
}

class TranscriptBubble extends StatelessWidget {
  const TranscriptBubble({super.key, required this.line});
  final TranscriptLine line;

  @override
  Widget build(BuildContext context) {
    final rider = line.role == 'rider';
    final system = line.role == 'system';
    return Align(
      alignment: rider ? Alignment.centerRight : Alignment.centerLeft,
      child: Container(
        margin: const EdgeInsets.symmetric(vertical: 3),
        padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 8),
        constraints: const BoxConstraints(maxWidth: 520),
        decoration: BoxDecoration(
          color: rider ? HexaColors.cyan.withValues(alpha: 0.16) : (system ? HexaColors.bad.withValues(alpha: 0.12) : HexaColors.surface),
          borderRadius: BorderRadius.circular(12),
        ),
        child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
          Text(rider ? 'Rider' : (system ? 'System' : 'HexaGo Safety'),
              style: const TextStyle(color: HexaColors.textMuted, fontSize: 10.5, fontWeight: FontWeight.w600)),
          Text(line.text, style: const TextStyle(fontSize: 13)),
        ]),
      ),
    );
  }
}

/// Vertical timeline: one dot per step, actor and time on the side.
class IncidentTimeline extends StatelessWidget {
  const IncidentTimeline({super.key, required this.entries});
  final List<TimelineEntry> entries;

  static Color actorColor(String actor) => switch (actor) {
        'rider' => HexaColors.cyan,
        'ops_supervisor' => HexaColors.good,
        'safety_detector' => HexaColors.amber,
        _ => HexaColors.incident,
      };

  @override
  Widget build(BuildContext context) {
    if (entries.isEmpty) {
      return const Text('No steps yet.', style: TextStyle(color: HexaColors.textMuted, fontSize: 12));
    }
    return Column(key: const ValueKey('incident-timeline'), children: [
      for (var i = 0; i < entries.length; i++)
        IntrinsicHeight(
          key: ValueKey('timeline-${entries[i].step}-$i'),
          child: Row(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
            SizedBox(
              width: 22,
              child: Column(children: [
                Container(
                  width: 12,
                  height: 12,
                  margin: const EdgeInsets.only(top: 3),
                  decoration: BoxDecoration(
                    shape: BoxShape.circle,
                    color: entries[i].failed ? HexaColors.bad : actorColor(entries[i].actor),
                  ),
                ),
                if (i < entries.length - 1) Expanded(child: Container(width: 2, color: HexaColors.border)),
              ]),
            ),
            const SizedBox(width: 8),
            Expanded(
              child: Padding(
                padding: const EdgeInsets.only(bottom: 12),
                child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  Text(entries[i].summary,
                      style: TextStyle(fontSize: 13, color: entries[i].failed ? HexaColors.bad : null)),
                  const SizedBox(height: 2),
                  Text(
                    '${entries[i].actor.replaceAll('_', ' ')}${entries[i].at == null ? '' : ' · ${formatClock(entries[i].at!)}'}',
                    style: const TextStyle(color: HexaColors.textMuted, fontSize: 11),
                  ),
                ]),
              ),
            ),
          ]),
        ),
    ]);
  }
}

// ---- path replay ----

/// Metres from [p] to the nearest segment of [plan] (local flat projection).
double deviationMeters(PathPoint p, List<PathPoint> plan) {
  if (plan.isEmpty) return 0;
  final kx = 111320 * math.cos(p.lat * math.pi / 180);
  const ky = 110574.0;
  if (plan.length == 1) {
    return math.sqrt(math.pow((plan[0].lng - p.lng) * kx, 2) + math.pow((plan[0].lat - p.lat) * ky, 2));
  }
  var best = double.infinity;
  for (var i = 1; i < plan.length; i++) {
    final ax = (plan[i - 1].lng - p.lng) * kx, ay = (plan[i - 1].lat - p.lat) * ky;
    final bx = (plan[i].lng - p.lng) * kx, by = (plan[i].lat - p.lat) * ky;
    final dx = bx - ax, dy = by - ay;
    final l = dx * dx + dy * dy;
    final t = l > 0 ? (-(ax * dx + ay * dy) / l).clamp(0.0, 1.0) : 0.0;
    best = math.min(best, math.sqrt(math.pow(ax + t * dx, 2) + math.pow(ay + t * dy, 2)));
  }
  return best;
}

/// Splits [path] into runs that are on route or off route (> [deviationM]).
List<({bool off, List<PathPoint> points})> deviationRuns(List<PathPoint> path, List<PathPoint> plan, double deviationM) {
  final runs = <({bool off, List<PathPoint> points})>[];
  for (final p in path) {
    final off = plan.length >= 2 && deviationMeters(p, plan) > deviationM;
    if (runs.isEmpty || runs.last.off != off) {
      // Start the new run at the previous point so the line stays continuous.
      runs.add((off: off, points: [if (runs.isNotEmpty) runs.last.points.last, p]));
    } else {
      runs.last.points.add(p);
    }
  }
  return runs;
}

class PathReplay extends ConsumerStatefulWidget {
  const PathReplay({super.key, required this.plan, required this.path, this.deviationM = 300, this.step = const Duration(milliseconds: 150)});
  final List<PathPoint> plan;
  final List<PathPoint> path;
  final double deviationM;
  final Duration step;

  @override
  ConsumerState<PathReplay> createState() => _PathReplayState();
}

class _PathReplayState extends ConsumerState<PathReplay> {
  final _map = MapController();
  int _index = 0;
  Timer? _timer;

  @override
  void initState() {
    super.initState();
    _index = math.max(0, widget.path.length - 1);
  }

  @override
  void didUpdateWidget(PathReplay old) {
    super.didUpdateWidget(old);
    if (_timer == null) _index = math.max(0, widget.path.length - 1);
  }

  @override
  void dispose() {
    _timer?.cancel();
    _map.dispose();
    super.dispose();
  }

  void _togglePlay() {
    if (_timer != null) {
      _timer!.cancel();
      setState(() => _timer = null);
      return;
    }
    setState(() => _index = 0);
    _timer = Timer.periodic(widget.step, (t) {
      if (!mounted) return;
      if (_index >= widget.path.length - 1) {
        t.cancel();
        setState(() => _timer = null);
        return;
      }
      setState(() => _index++);
    });
  }

  @override
  Widget build(BuildContext context) {
    final all = [...widget.plan, ...widget.path];
    if (all.isEmpty) {
      return const Center(child: Text('No vehicle path recorded.', style: TextStyle(color: HexaColors.textMuted)));
    }
    final runs = deviationRuns(widget.path, widget.plan, widget.deviationM);
    final offCount = runs.where((r) => r.off).length;
    final showTiles = ref.watch(showMapTilesProvider);
    final basemap = showTiles ? ref.watch(basemapProvider).value : null;
    final cur = widget.path.isEmpty ? null : widget.path[_index.clamp(0, widget.path.length - 1)];
    final bounds = LatLngBounds.fromPoints([for (final p in all) LatLng(p.lat, p.lng)]);
    return Column(key: const ValueKey('path-replay'), children: [
      Expanded(
        child: ClipRRect(
          borderRadius: BorderRadius.circular(10),
          child: FlutterMap(
            mapController: _map,
            options: MapOptions(
              initialCameraFit: CameraFit.bounds(bounds: bounds, padding: const EdgeInsets.all(30), maxZoom: 16),
              backgroundColor: HexaColors.surface,
            ),
            children: [
              if (basemap != null)
                TileLayer(
                  urlTemplate: basemap.urlTemplate,
                  fallbackUrl: basemap.fallbackUrl,
                  subdomains: basemap.subdomains,
                  tileBuilder: basemap.darkFilter ? darkModeTileBuilder : null,
                  userAgentPackageName: 'com.hexago.hexago_app',
                  maxZoom: 19,
                ),
              PolylineLayer(polylines: [
                if (widget.plan.length >= 2)
                  Polyline(points: [for (final p in widget.plan) LatLng(p.lat, p.lng)], color: Colors.white54, strokeWidth: 4),
                for (final r in runs)
                  if (r.points.length >= 2)
                    Polyline(
                      points: [for (final p in r.points) LatLng(p.lat, p.lng)],
                      color: r.off ? HexaColors.bad : HexaColors.cyan,
                      strokeWidth: r.off ? 5 : 3.5,
                    ),
              ]),
              if (cur != null)
                MarkerLayer(markers: [
                  Marker(
                    point: LatLng(cur.lat, cur.lng),
                    width: 18,
                    height: 18,
                    child: Container(
                      key: const ValueKey('replay-marker'),
                      decoration: BoxDecoration(shape: BoxShape.circle, color: HexaColors.amber, border: Border.all(color: Colors.black, width: 2)),
                    ),
                  ),
                ]),
            ],
          ),
        ),
      ),
      Row(children: [
        IconButton(
          key: const ValueKey('replay-play'),
          tooltip: _timer == null ? 'Replay the vehicle path' : 'Pause',
          onPressed: widget.path.length < 2 ? null : _togglePlay,
          icon: Icon(_timer == null ? Icons.play_arrow : Icons.pause),
        ),
        Expanded(
          child: Slider(
            key: const ValueKey('replay-slider'),
            value: _index.toDouble(),
            max: math.max(1, widget.path.length - 1).toDouble(),
            onChanged: widget.path.length < 2 ? null : (v) => setState(() => _index = v.round()),
          ),
        ),
        Text(
          cur == null ? '' : '${cur.speedKmh.round()} km/h',
          style: const TextStyle(color: HexaColors.textMuted, fontSize: 11),
        ),
        const SizedBox(width: 8),
        Text(offCount == 0 ? 'on route' : 'deviation in red',
            key: const ValueKey('replay-legend'),
            style: TextStyle(color: offCount == 0 ? HexaColors.textMuted : HexaColors.bad, fontSize: 11)),
        const SizedBox(width: 4),
      ]),
    ]);
  }
}
