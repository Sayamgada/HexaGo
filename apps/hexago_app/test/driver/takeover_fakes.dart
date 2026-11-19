import 'dart:async';

import 'package:hexago_app/features/driver/data/takeover_api.dart';
import 'package:hexago_app/features/driver/models/takeover_models.dart';

/// Records take-over calls; offers are pushed with [emit].
class FakeTakeoverApi implements TakeoverApi {
  FakeTakeoverApi({this.cells = const [], this.failTakeover = false, this.rematchTo = 'd_017', this.stateOffer});

  final List<ForecastCell> cells;
  final bool failTakeover;
  final String rematchTo;
  RideOffer? stateOffer;
  final calls = <String>[];
  final _offers = StreamController<RideOffer>.broadcast();

  void emit(RideOffer o) => _offers.add(o);

  @override
  Future<ForecastHeatmap> heatmap(int horizon) async {
    calls.add('heatmap:$horizon');
    return ForecastHeatmap(horizon: horizon, cells: cells, scenarios: horizon == 0 ? const [] : const ['rain_storm']);
  }

  @override
  Future<TakeoverState> takeOver(String driverId) async {
    calls.add('takeover:$driverId');
    if (failTakeover) throw StateError('disabled');
    return TakeoverState(driverId: driverId, humanControlled: true, ttlS: 600, offer: stateOffer);
  }

  @override
  Future<void> release(String driverId) async => calls.add('release:$driverId');

  @override
  Future<void> sendRide(String driverId) async => calls.add('send:$driverId');

  @override
  Future<OfferResult> respond(String rideId, {required bool accept}) async {
    calls.add('${accept ? 'accept' : 'decline'}:$rideId');
    return OfferResult(rideId: rideId, decision: accept ? 'accepted' : 'declined', rematchedTo: accept ? '' : rematchTo);
  }

  @override
  Future<RoutePath> route(double fromLat, double fromLng, double toLat, double toLng) async {
    calls.add('route');
    return RoutePath(points: [(fromLat, fromLng), (toLat, fromLng), (toLat, toLng)], source: 'osrm', distanceKm: 2);
  }

  @override
  Stream<RideOffer> offers(String driverId) => _offers.stream;
}

ForecastCell cell(String id, double lat, double lng, {double score = 0.5, int earn = 200, String src = 'template'}) =>
    ForecastCell(
      h3: id,
      boundary: [(lat + 0.004, lng), (lat, lng + 0.004), (lat - 0.004, lng), (lat, lng - 0.004)],
      centerLat: lat,
      centerLng: lng,
      demandScore: score,
      requestsPerHr: score * 24,
      earningsPerHr: earn,
      explanation: 'Explanation for $id',
      explanationSource: src,
    );

const offer1 = RideOffer(
  type: 'ride_offer', rideId: 'ride-0001-aaaa', driverId: 'd_002', pickupLat: 12.975, pickupLng: 77.6,
  dropLat: 13.0, dropLng: 77.62, fareInr: 214, tripKm: 4.6, pickupEtaSec: 150, timeoutS: 15,
);
