import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../theme/hexago_theme.dart';
import '../../util/errors.dart';
import '../../widgets/waking_gate.dart';
import '../ops/models/ops_models.dart';
import 'data/takeover_api.dart';
import 'models/driver_models.dart';
import 'models/takeover_models.dart';
import 'state/driver_providers.dart';
import 'widgets/driver_earnings.dart';
import 'widgets/driver_map.dart';
import 'widgets/driver_widgets.dart';
import 'widgets/forecast_widgets.dart';
import 'widgets/ride_offer_card.dart';

/// `/driver/:id`: the taken-over driver's home (live map, status, ride, AV
/// panel, Safety freeze) and earnings.
///
/// Opening it takes the car over (take-over mode): dispatch offers rides to
/// the person instead of auto-accepting them (15 s to accept), the take-over
/// is renewed every minute while the screen is open and released when it
/// closes. The Demand Forecast heatmap and "Move to hotspot" cards show where
/// demand is expected now, in 15 and in 30 minutes.
class DriverHomeScreen extends StatelessWidget {
  const DriverHomeScreen({super.key, required this.driverId, this.showLiveMap = true});

  final String driverId;
  final bool showLiveMap;

  @override
  Widget build(BuildContext context) => WakingGate(child: DriverHomeBody(driverId: driverId, showLiveMap: showLiveMap));
}

class DriverHomeBody extends ConsumerStatefulWidget {
  const DriverHomeBody({super.key, required this.driverId, this.showLiveMap = true, this.takeover = true});

  final String driverId;
  final bool showLiveMap;

  /// Take the car over while the screen is open (off: read-only view).
  final bool takeover;

  @override
  ConsumerState<DriverHomeBody> createState() => _DriverHomeBodyState();
}

class _DriverHomeBodyState extends ConsumerState<DriverHomeBody> {
  DriverDetail? _detail;
  DriverPosition? _live; // newest position from the drivers stream
  Object? _error;
  Timer? _timer;
  StreamSubscription<List<DriverPosition>>? _sub;

  // Take-over mode.
  late final TakeoverApi _api;
  TakeoverState? _takeover;
  Timer? _renew;
  StreamSubscription<RideOffer>? _offerSub;
  RideOffer? _offer;
  bool _responding = false;
  bool _sending = false;

  // Demand Forecast.
  int _horizon = 0;
  ForecastHeatmap? _heatmap;
  Timer? _forecastTimer;
  Hotspot? _target;
  List<(double, double)> _route = const [];

  @override
  void initState() {
    super.initState();
    _api = ref.read(takeoverApiProvider);
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
    if (widget.takeover) {
      unawaited(_takeOver());
      _renew = Timer.periodic(ref.read(takeoverRenewProvider), (_) => _takeOver());
      _offerSub = _api.offers(widget.driverId).listen(_onOffer, onError: (Object _) {});
      unawaited(_loadForecast());
      _forecastTimer = Timer.periodic(ref.read(forecastPollProvider), (_) => _loadForecast());
    }
  }

  @override
  void dispose() {
    _timer?.cancel();
    _sub?.cancel();
    _renew?.cancel();
    _offerSub?.cancel();
    _forecastTimer?.cancel();
    if (widget.takeover && _takeover?.humanControlled == true) {
      unawaited(_api.release(widget.driverId).catchError((Object _) {})); // back to the simulator
    }
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

  /// Take over (or renew); also picks up an offer the stream may have missed.
  Future<void> _takeOver() async {
    try {
      final t = await _api.takeOver(widget.driverId);
      if (!mounted) return;
      setState(() {
        _takeover = t;
        if (_offer == null && t.offer != null && t.offer!.isOffer) _offer = t.offer;
      });
    } catch (_) {
      // Take-over unavailable (TAKEOVER_ENABLED=false, waking gateway): the screen stays read-only.
    }
  }

  void _onOffer(RideOffer o) {
    if (!mounted) return;
    if (o.isOffer) {
      setState(() => _offer = o);
      return;
    }
    if (_offer?.rideId != o.rideId) return;
    setState(() => _offer = null);
    if (o.reason != 'accepted' && o.reason != 'declined') _snack(offerClosedMessage(o));
  }

  Future<void> _respond(bool accept) async {
    final o = _offer;
    if (o == null) return;
    setState(() => _responding = true);
    try {
      final r = await _api.respond(o.rideId, accept: accept);
      if (!mounted) return;
      setState(() => _offer = null);
      if (accept) {
        _snack('Ride accepted: head to the pickup');
        unawaited(_load());
      } else {
        _snack(r.rematchedTo.isNotEmpty ? 'Declined: re-matched to ${r.rematchedTo}' : 'Declined: no other driver free yet');
      }
    } catch (e) {
      if (!mounted) return;
      setState(() => _offer = null); // the offer is gone (timed out or cancelled meanwhile)
      _snack(friendlyError(e, action: accept ? 'accept the ride' : 'decline the ride'));
    } finally {
      if (mounted) setState(() => _responding = false);
    }
  }

  Future<void> _sendRide() async {
    setState(() => _sending = true);
    try {
      await _api.sendRide(widget.driverId);
      // The offer arrives on the stream; check the take-over state too in case the socket missed it.
      Future<void>.delayed(const Duration(seconds: 2), () {
        if (mounted && _offer == null) unawaited(_takeOver());
      });
    } catch (e) {
      if (mounted) _snack(friendlyError(e, action: 'send a demo ride'));
    } finally {
      if (mounted) setState(() => _sending = false);
    }
  }

  Future<void> _loadForecast() async {
    final h = _horizon;
    try {
      final m = await _api.heatmap(h);
      if (mounted && h == _horizon) setState(() => _heatmap = m);
    } catch (_) {
      // The heatmap is optional: keep the last one.
    }
  }

  void _setHorizon(int h) {
    setState(() => _horizon = h);
    unawaited(_loadForecast());
  }

  Future<void> _goTo(Hotspot h, double lat, double lng) async {
    if (_target?.cell.h3 == h.cell.h3) {
      setState(() {
        _target = null;
        _route = const [];
      });
      return;
    }
    setState(() {
      _target = h;
      _route = [(lat, lng), (h.cell.centerLat, h.cell.centerLng)];
    });
    try {
      final r = await _api.route(lat, lng, h.cell.centerLat, h.cell.centerLng);
      if (mounted && _target?.cell.h3 == h.cell.h3 && r.points.length >= 2) setState(() => _route = r.points);
    } catch (_) {
      // keep the straight line
    }
  }

  void _snack(String msg) => ScaffoldMessenger.maybeOf(context)
    ?..removeCurrentSnackBar()
    ..showSnackBar(SnackBar(content: Text(msg)));

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
    final lat = live?.lat ?? d.summary.lat, lng = live?.lng ?? d.summary.lng;
    final human = _takeover?.humanControlled == true;
    final cells = _heatmap?.cells ?? const <ForecastCell>[];
    final offer = _offer;
    final side = <Widget>[
      if (offer != null) ...[
        RideOfferCard(
          offer: offer,
          driverLat: lat,
          driverLng: lng,
          busy: _responding,
          onAccept: () => _respond(true),
          onDecline: () => _respond(false),
        ),
        const SizedBox(height: 10),
      ],
      if (d.suspended) ...[
        FrozenBanner(incidentId: d.incidentId, reason: d.suspensionReason, since: d.suspendedAt),
        const SizedBox(height: 10),
      ],
      if (human) ...[_humanBanner(d), const SizedBox(height: 10)],
      RideCard(ride: d.ride),
      if (d.avStatus != null) ...[const SizedBox(height: 10), AvPanel(status: d.avStatus!)],
      if (widget.takeover) ...[
        const SizedBox(height: 14),
        Row(children: [
          const Icon(Icons.local_fire_department_outlined, size: 18, color: HexaColors.amber),
          const SizedBox(width: 6),
          const Expanded(child: Text('Move to hotspot', style: TextStyle(fontWeight: FontWeight.w700))),
          if (!widget.showLiveMap) HorizonSelector(value: _horizon, onChanged: _setHorizon),
        ]),
        if ((_heatmap?.scenarios ?? const []).isNotEmpty)
          Padding(
            padding: const EdgeInsets.only(top: 4),
            child: Text('Active: ${_heatmap!.scenarios.join(', ')}',
                key: const ValueKey('forecast-scenarios'), style: const TextStyle(fontSize: 11, color: HexaColors.amber)),
          ),
        const SizedBox(height: 6),
        HotspotCards(
          key: const ValueKey('hotspot-cards'),
          hotspots: topHotspots(cells, lat, lng),
          selected: _target?.cell.h3,
          loading: _heatmap == null,
          onTap: (h) => _goTo(h, lat, lng),
        ),
      ],
      const SizedBox(height: 10),
      Text(
        '${d.speedKmh.round()} km/h · ★ ${d.summary.rating.toStringAsFixed(1)}',
        style: const TextStyle(color: HexaColors.textMuted, fontSize: 12),
      ),
    ];
    final map = widget.showLiveMap
        ? DriverMap(
            lat: lat,
            lng: lng,
            heading: live?.heading ?? d.heading,
            phase: d.phase,
            ride: d.ride,
            av: d.summary.av,
            forecast: cells,
            horizon: _horizon,
            onHorizon: widget.takeover ? _setHorizon : null,
            selectedCell: _target?.cell.h3,
            route: _route,
            offer: offer,
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

  Widget _humanBanner(DriverDetail d) {
    final idle = d.ride == null && _offer == null && !d.suspended;
    return Container(
      key: const ValueKey('takeover-banner'),
      padding: const EdgeInsets.all(10),
      decoration: BoxDecoration(
        color: HexaColors.cyan.withValues(alpha: 0.08),
        borderRadius: BorderRadius.circular(10),
        border: Border.all(color: HexaColors.cyan.withValues(alpha: 0.5)),
      ),
      child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
        const Row(children: [
          Icon(Icons.sports_motorsports_outlined, size: 18, color: HexaColors.cyan),
          SizedBox(width: 6),
          Expanded(
            child: Text('You are driving this car. Ride requests come to you with 15 s to accept.',
                style: TextStyle(fontSize: 12)),
          ),
        ]),
        const SizedBox(height: 8),
        OutlinedButton.icon(
          key: const ValueKey('send-ride'),
          onPressed: idle && !_sending ? _sendRide : null,
          icon: _sending
              ? const SizedBox(width: 14, height: 14, child: CircularProgressIndicator(strokeWidth: 2))
              : const Icon(Icons.hail, size: 18),
          label: const Text('Send me a ride (demo)'),
        ),
      ]),
    );
  }
}
