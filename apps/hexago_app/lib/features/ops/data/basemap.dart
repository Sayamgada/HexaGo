import 'dart:typed_data';

import 'package:flutter_map/flutter_map.dart' show LatLngBounds;
import 'package:latlong2/latlong.dart';

/// A raster basemap for the fleet map.
class Basemap {
  const Basemap._({
    required this.name,
    required this.urlTemplate,
    required this.attribution,
    this.subdomains = const [],
    this.fallbackUrl,
    this.darkFilter = false,
  });

  final String name;
  final String urlTemplate;
  final List<String> subdomains;
  final String attribution;

  /// Per-tile fallback when a tile request fails outright.
  final String? fallbackUrl;

  /// Light tiles are darkened to suit the dark UI.
  final bool darkFilter;

  /// Keyless CARTO dark_all raster tiles.
  static const cartoDark = Basemap._(
    name: 'carto-dark',
    urlTemplate: 'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png',
    subdomains: ['a', 'b', 'c', 'd'],
    attribution: '© OpenStreetMap contributors © CARTO',
    fallbackUrl: 'https://tile.openstreetmap.org/{z}/{x}/{y}.png',
  );

  /// Standard OpenStreetMap tiles.
  static const osm = Basemap._(
    name: 'osm',
    urlTemplate: 'https://tile.openstreetmap.org/{z}/{x}/{y}.png',
    attribution: '© OpenStreetMap contributors',
    darkFilter: true,
  );

  @override
  String toString() => 'Basemap($name)';
}

/// Fetches the bytes at a URL (throws on HTTP or network errors).
typedef TileFetcher = Future<Uint8List> Function(String url);

/// Two Bengaluru tiles at different zooms; real tiles always differ.
const _probeTiles = [
  'https://a.basemaps.cartocdn.com/dark_all/12/2930/1891.png',
  'https://a.basemaps.cartocdn.com/dark_all/9/366/237.png',
];

/// Decides between CARTO and OSM once per session.
///
/// Keyless CARTO requests can answer HTTP 200 with the same "API KEY
/// REQUIRED" image for every tile, which a per-tile fallback never notices.
/// So the probe loads two different tiles: if either fails, is empty, or both
/// are byte-identical (the placeholder), the map uses OSM instead.
Future<Basemap> probeBasemap(TileFetcher fetch) async {
  try {
    final a = await fetch(_probeTiles[0]);
    final b = await fetch(_probeTiles[1]);
    if (a.isEmpty || b.isEmpty || _same(a, b)) return Basemap.osm;
    return Basemap.cartoDark;
  } catch (_) {
    return Basemap.osm;
  }
}

bool _same(Uint8List a, Uint8List b) {
  if (a.length != b.length) return false;
  for (var i = 0; i < a.length; i++) {
    if (a[i] != b[i]) return false;
  }
  return true;
}

/// Bounds around every point, padded slightly so a single car still yields
/// a fittable box. Null when there are no points.
LatLngBounds? fleetBounds(Iterable<LatLng> points) {
  if (points.isEmpty) return null;
  var s = 90.0, n = -90.0, w = 180.0, e = -180.0;
  for (final p in points) {
    if (p.latitude < s) s = p.latitude;
    if (p.latitude > n) n = p.latitude;
    if (p.longitude < w) w = p.longitude;
    if (p.longitude > e) e = p.longitude;
  }
  const pad = 0.005; // ~500 m
  if (n - s < pad) {
    s -= pad;
    n += pad;
  }
  if (e - w < pad) {
    w -= pad;
    e += pad;
  }
  return LatLngBounds(LatLng(s, w), LatLng(n, e));
}
