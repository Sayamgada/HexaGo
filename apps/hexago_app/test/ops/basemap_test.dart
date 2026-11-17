import 'dart:typed_data';

import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/ops/data/basemap.dart';
import 'package:latlong2/latlong.dart';

void main() {
  group('Basemap definitions', () {
    test('CARTO dark_all needs no key', () {
      expect(Basemap.cartoDark.urlTemplate, 'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png');
      expect(Basemap.cartoDark.urlTemplate.contains('key'), isFalse);
      expect(Basemap.cartoDark.subdomains, ['a', 'b', 'c', 'd']);
      expect(Basemap.cartoDark.attribution, '© OpenStreetMap contributors © CARTO');
      expect(Basemap.cartoDark.fallbackUrl, Basemap.osm.urlTemplate);
    });
    test('OSM fallback', () {
      expect(Basemap.osm.urlTemplate, 'https://tile.openstreetmap.org/{z}/{x}/{y}.png');
      expect(Basemap.osm.attribution, '© OpenStreetMap contributors');
      expect(Basemap.osm.darkFilter, isTrue);
    });
  });

  group('probeBasemap', () {
    Uint8List bytes(List<int> b) => Uint8List.fromList(b);

    test('distinct CARTO tiles -> CARTO', () async {
      final b = await probeBasemap((url) async => bytes(url.contains('/12/') ? [1, 2, 3] : [4, 5, 6]));
      expect(b, Basemap.cartoDark);
    });
    test('identical tiles (the "API KEY REQUIRED" placeholder) -> OSM', () async {
      final b = await probeBasemap((url) async => bytes([9, 9, 9]));
      expect(b, Basemap.osm);
    });
    test('network error -> OSM', () async {
      final b = await probeBasemap((url) async => throw Exception('offline'));
      expect(b, Basemap.osm);
    });
    test('empty response -> OSM', () async {
      final b = await probeBasemap((url) async => Uint8List(0));
      expect(b, Basemap.osm);
    });
    test('probes two different CARTO tiles', () async {
      final urls = <String>[];
      await probeBasemap((url) async {
        urls.add(url);
        return bytes([urls.length]);
      });
      expect(urls, hasLength(2));
      expect(urls.toSet(), hasLength(2));
      expect(urls.every((u) => u.startsWith('https://a.basemaps.cartocdn.com/dark_all/')), isTrue);
    });
  });

  group('fleetBounds', () {
    test('null without points', () => expect(fleetBounds(const []), isNull));
    test('covers every point', () {
      final b = fleetBounds(const [LatLng(12.84, 77.66), LatLng(13.20, 77.71), LatLng(12.97, 77.59)])!;
      expect(b.south, 12.84);
      expect(b.north, 13.20);
      expect(b.west, 77.59);
      expect(b.east, 77.71);
    });
    test('a single point is padded so the camera can fit it', () {
      final b = fleetBounds(const [LatLng(12.97, 77.59)])!;
      expect(b.north, greaterThan(b.south));
      expect(b.east, greaterThan(b.west));
    });
  });
}
