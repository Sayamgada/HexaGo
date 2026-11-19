import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:url_launcher/url_launcher.dart';

/// Opens a link outside the app (browser); tests override [linkOpenerProvider].
typedef LinkOpener = Future<bool> Function(Uri uri);

final linkOpenerProvider = Provider<LinkOpener>(
  (ref) => (uri) => launchUrl(uri, mode: LaunchMode.externalApplication, webOnlyWindowName: '_blank'),
);
