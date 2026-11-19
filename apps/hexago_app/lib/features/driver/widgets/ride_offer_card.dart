import 'dart:async';

import 'package:flutter/material.dart';

import '../../../theme/hexago_theme.dart';
import '../../../util/format.dart';
import '../models/driver_models.dart' show formatEta;
import '../models/takeover_models.dart';

/// A ride request for the taken-over driver: pickup, drop, fare, ETA to the
/// pickup, a countdown (15 s) and Accept / Decline. When the countdown runs
/// out the buttons disable; dispatch re-matches the ride on its own.
class RideOfferCard extends StatefulWidget {
  const RideOfferCard({
    super.key,
    required this.offer,
    required this.onAccept,
    required this.onDecline,
    this.driverLat,
    this.driverLng,
    this.busy = false,
  });

  final RideOffer offer;
  final VoidCallback onAccept;
  final VoidCallback onDecline;
  final double? driverLat;
  final double? driverLng;
  final bool busy;

  @override
  State<RideOfferCard> createState() => _RideOfferCardState();
}

class _RideOfferCardState extends State<RideOfferCard> {
  int _elapsedMs = 0; // counted by the ticker, so tests can drive it with fake time
  Timer? _tick;

  @override
  void initState() {
    super.initState();
    _start();
  }

  void _start() {
    _elapsedMs = 0;
    _tick?.cancel();
    _tick = Timer.periodic(const Duration(milliseconds: 250), (t) {
      if (!mounted) return;
      setState(() => _elapsedMs += 250);
      if (_left <= 0) t.cancel();
    });
  }

  @override
  void didUpdateWidget(covariant RideOfferCard old) {
    super.didUpdateWidget(old);
    if (old.offer.rideId != widget.offer.rideId) _start();
  }

  @override
  void dispose() {
    _tick?.cancel();
    super.dispose();
  }

  double get _left => (widget.offer.timeoutS - _elapsedMs / 1000).clamp(0, widget.offer.timeoutS.toDouble());

  String _pt(double lat, double lng) => '${lat.toStringAsFixed(4)}, ${lng.toStringAsFixed(4)}';

  @override
  Widget build(BuildContext context) {
    final o = widget.offer;
    final left = _left;
    final expired = left <= 0;
    final away = widget.driverLat == null
        ? ''
        : ' · ${haversineKm(widget.driverLat!, widget.driverLng!, o.pickupLat, o.pickupLng).toStringAsFixed(1)} km away';
    return Container(
      key: const ValueKey('offer-card'),
      padding: const EdgeInsets.all(14),
      decoration: BoxDecoration(
        color: HexaColors.panel,
        borderRadius: BorderRadius.circular(12),
        border: Border.all(color: HexaColors.amber, width: 1.6),
        boxShadow: const [BoxShadow(color: Color(0x55FFB020), blurRadius: 18)],
      ),
      child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
        Row(children: [
          const Icon(Icons.notifications_active, color: HexaColors.amber, size: 20),
          const SizedBox(width: 8),
          const Text('New ride request', style: TextStyle(fontWeight: FontWeight.w800, fontSize: 16)),
          const SizedBox(width: 6),
          Text(shortId(o.rideId), style: const TextStyle(fontFamily: 'monospace', color: HexaColors.textMuted, fontSize: 12)),
          const Spacer(),
          Text(expired ? 'Timed out' : '${left.ceil()} s',
              key: const ValueKey('offer-countdown'),
              style: TextStyle(color: expired || left < 5 ? HexaColors.bad : HexaColors.amber, fontWeight: FontWeight.w800)),
        ]),
        const SizedBox(height: 8),
        ClipRRect(
          borderRadius: BorderRadius.circular(4),
          child: LinearProgressIndicator(
            value: o.timeoutS == 0 ? 0 : left / o.timeoutS,
            minHeight: 5,
            color: left < 5 ? HexaColors.bad : HexaColors.amber,
            backgroundColor: HexaColors.border,
          ),
        ),
        const SizedBox(height: 10),
        Row(children: [
          _stat('Fare', '₹${o.fareInr.toStringAsFixed(0)}', HexaColors.good, 'offer-fare'),
          _stat('To pickup', formatEta(o.pickupEtaSec), HexaColors.toPickup, 'offer-eta'),
          _stat('Trip', '${o.tripKm.toStringAsFixed(1)} km', HexaColors.onTrip, 'offer-trip'),
        ]),
        const SizedBox(height: 10),
        _line(Icons.trip_origin, 'Pickup', '${_pt(o.pickupLat, o.pickupLng)}$away', HexaColors.toPickup),
        _line(Icons.flag_outlined, 'Drop', _pt(o.dropLat, o.dropLng), HexaColors.onTrip),
        const SizedBox(height: 12),
        Row(children: [
          Expanded(
            child: OutlinedButton.icon(
              key: const ValueKey('offer-decline'),
              onPressed: expired || widget.busy ? null : widget.onDecline,
              icon: const Icon(Icons.close),
              label: const Text('Decline'),
            ),
          ),
          const SizedBox(width: 10),
          Expanded(
            flex: 2,
            child: FilledButton.icon(
              key: const ValueKey('offer-accept'),
              onPressed: expired || widget.busy ? null : widget.onAccept,
              style: FilledButton.styleFrom(backgroundColor: HexaColors.good, foregroundColor: Colors.black),
              icon: const Icon(Icons.check),
              label: const Text('Accept'),
            ),
          ),
        ]),
      ]),
    );
  }

  Widget _stat(String label, String value, Color c, String key) => Expanded(
        child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
          Text(label, style: const TextStyle(fontSize: 11, color: HexaColors.textMuted)),
          Text(value, key: ValueKey(key), style: TextStyle(fontSize: 18, fontWeight: FontWeight.w800, color: c)),
        ]),
      );

  Widget _line(IconData icon, String label, String value, Color c) => Padding(
        padding: const EdgeInsets.only(top: 3),
        child: Row(children: [
          Icon(icon, size: 15, color: c),
          const SizedBox(width: 6),
          SizedBox(width: 52, child: Text(label, style: const TextStyle(fontSize: 12, color: HexaColors.textMuted))),
          Expanded(child: Text(value, style: const TextStyle(fontSize: 12, fontFamily: 'monospace'))),
        ]),
      );
}
