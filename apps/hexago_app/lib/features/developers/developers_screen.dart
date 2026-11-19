import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../config/app_config.dart';
import '../../theme/hexago_theme.dart';
import '../../util/links.dart';
import 'developer_api.dart';

final developerApiProvider = Provider<DeveloperApi>((ref) => DioDeveloperApi(apiBaseUrl: AppConfig.apiBaseUrl));

/// Developers: create an API key, copy curl examples, open the API reference.
class DevelopersScreen extends ConsumerStatefulWidget {
  const DevelopersScreen({super.key});

  @override
  ConsumerState<DevelopersScreen> createState() => _DevelopersScreenState();
}

class _DevelopersScreenState extends ConsumerState<DevelopersScreen> {
  final _name = TextEditingController(text: 'my app');
  final Set<String> _scopes = {...apiScopes.keys};
  CreatedKey? _key;
  String? _error;
  bool _busy = false;

  @override
  void dispose() {
    _name.dispose();
    super.dispose();
  }

  Future<void> _create() async {
    setState(() {
      _busy = true;
      _error = null;
    });
    try {
      final k = await ref.read(developerApiProvider).createKey(
            name: _name.text.trim().isEmpty ? 'my app' : _name.text.trim(),
            scopes: apiScopes.keys.where(_scopes.contains).toList(),
          );
      if (mounted) setState(() => _key = k);
    } catch (e) {
      if (mounted) setState(() => _error = e.toString());
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  Future<void> _copy(String text, String what) async {
    await Clipboard.setData(ClipboardData(text: text));
    if (!mounted) return;
    ScaffoldMessenger.of(context)
      ..hideCurrentSnackBar()
      ..showSnackBar(SnackBar(content: Text('$what copied'), duration: const Duration(seconds: 2)));
  }

  @override
  Widget build(BuildContext context) {
    final api = ref.watch(developerApiProvider);
    final open = ref.watch(linkOpenerProvider);
    final docsUrl = '${api.baseUrl}/docs';
    return Scaffold(
      appBar: AppBar(
        title: const Row(children: [
          Icon(Icons.code, color: HexaColors.amber),
          SizedBox(width: 8),
          Text('Developers', style: TextStyle(fontWeight: FontWeight.bold)),
        ]),
        actions: [
          TextButton.icon(
            key: const Key('open-docs'),
            onPressed: () => open(Uri.parse(docsUrl)),
            icon: const Icon(Icons.menu_book_outlined, size: 18),
            label: const Text('API reference'),
          ),
          const SizedBox(width: 8),
        ],
      ),
      body: SingleChildScrollView(
        padding: const EdgeInsets.all(20),
        child: Center(
          child: ConstrainedBox(
            constraints: const BoxConstraints(maxWidth: 960),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                const Text('HexaGo Public API',
                    style: TextStyle(fontSize: 24, fontWeight: FontWeight.bold)),
                const SizedBox(height: 6),
                Text(
                  'Rides, the live stream, the booking agent, pricing, safety and the ledger through one gateway: '
                  '${api.baseUrl}. Keys are rate limited per key (60 requests/min by default) and every '
                  'response carries X-RateLimit-* headers.',
                  style: const TextStyle(color: HexaColors.textMuted, height: 1.4),
                ),
                const SizedBox(height: 20),
                _keyCard(),
                const SizedBox(height: 16),
                _examplesCard(),
                const SizedBox(height: 16),
                _linksCard(open, docsUrl, api.baseUrl),
              ],
            ),
          ),
        ),
      ),
    );
  }

  Widget _section(String title, IconData icon, List<Widget> children) => Card(
        color: HexaColors.panel,
        shape: RoundedRectangleBorder(
            borderRadius: BorderRadius.circular(14), side: const BorderSide(color: HexaColors.border)),
        child: Padding(
          padding: const EdgeInsets.all(18),
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Row(children: [
              Icon(icon, color: HexaColors.amber, size: 20),
              const SizedBox(width: 8),
              Text(title, style: const TextStyle(fontSize: 17, fontWeight: FontWeight.w600)),
            ]),
            const SizedBox(height: 12),
            ...children,
          ]),
        ),
      );

  Widget _keyCard() {
    final k = _key;
    return _section('1. Create an API key', Icons.vpn_key_outlined, [
      TextField(
        key: const Key('key-name'),
        controller: _name,
        decoration: const InputDecoration(labelText: 'Key name', isDense: true, border: OutlineInputBorder()),
      ),
      const SizedBox(height: 12),
      const Text('Scopes', style: TextStyle(color: HexaColors.textMuted, fontSize: 12)),
      const SizedBox(height: 6),
      Wrap(spacing: 8, runSpacing: 8, children: [
        for (final e in apiScopes.entries)
          Tooltip(
            message: e.value,
            child: FilterChip(
              key: Key('scope-${e.key}'),
              label: Text(e.key, style: const TextStyle(fontFamily: 'monospace', fontSize: 12)),
              selected: _scopes.contains(e.key),
              onSelected: (on) => setState(() => on ? _scopes.add(e.key) : _scopes.remove(e.key)),
            ),
          ),
      ]),
      const SizedBox(height: 14),
      FilledButton.icon(
        key: const Key('create-key'),
        onPressed: _busy || _scopes.isEmpty ? null : _create,
        icon: _busy
            ? const SizedBox(width: 16, height: 16, child: CircularProgressIndicator(strokeWidth: 2))
            : const Icon(Icons.add),
        label: Text(_busy ? 'Creating…' : 'Create key'),
      ),
      if (_error != null) ...[
        const SizedBox(height: 10),
        Text(_error!, key: const Key('key-error'), style: const TextStyle(color: HexaColors.bad)),
      ],
      if (k != null) ...[
        const SizedBox(height: 14),
        Container(
          padding: const EdgeInsets.all(12),
          decoration: BoxDecoration(
            color: HexaColors.background,
            borderRadius: BorderRadius.circular(10),
            border: Border.all(color: HexaColors.amber.withValues(alpha: 0.6)),
          ),
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Row(children: [
              Expanded(
                child: SelectableText(k.apiKey,
                    key: const Key('created-key'), style: const TextStyle(fontFamily: 'monospace', fontSize: 13)),
              ),
              IconButton(
                key: const Key('copy-key'),
                tooltip: 'Copy key',
                icon: const Icon(Icons.copy, size: 18),
                onPressed: () => _copy(k.apiKey, 'API key'),
              ),
            ]),
            const SizedBox(height: 6),
            Text(
              'Shown only once: HexaGo stores just its SHA-256. ${k.scopes.length} scopes, '
              '${k.rateLimitPerMin} requests/min.',
              style: const TextStyle(color: HexaColors.warn, fontSize: 12),
            ),
          ]),
        ),
      ],
    ]);
  }

  Widget _examplesCard() {
    final api = ref.read(developerApiProvider);
    final examples = curlExamples(api.baseUrl, _key?.apiKey);
    return _section('2. Try it with curl', Icons.terminal, [
      if (_key == null)
        const Padding(
          padding: EdgeInsets.only(bottom: 8),
          child: Text(r'Create a key above, or export HEXAGO_KEY=hxg_… first.',
              style: TextStyle(color: HexaColors.textMuted, fontSize: 12)),
        ),
      for (final (i, ex) in examples.indexed) ...[
        Row(children: [
          Expanded(child: Text(ex.title, style: const TextStyle(fontWeight: FontWeight.w600, fontSize: 13))),
          IconButton(
            key: Key('copy-example-$i'),
            tooltip: 'Copy',
            icon: const Icon(Icons.copy, size: 16),
            onPressed: () => _copy(ex.command, ex.title),
          ),
        ]),
        Container(
          width: double.infinity,
          padding: const EdgeInsets.all(10),
          margin: const EdgeInsets.only(bottom: 10),
          decoration: BoxDecoration(color: HexaColors.background, borderRadius: BorderRadius.circular(8)),
          child: SelectableText(ex.command,
              style: const TextStyle(fontFamily: 'monospace', fontSize: 12, color: Color(0xFFCBD5E1))),
        ),
      ],
    ]);
  }

  Widget _linksCard(LinkOpener open, String docsUrl, String baseUrl) => _section(
        '3. Reference and SDKs',
        Icons.menu_book_outlined,
        [
          Wrap(spacing: 10, runSpacing: 10, children: [
            OutlinedButton.icon(
              key: const Key('link-docs'),
              onPressed: () => open(Uri.parse(docsUrl)),
              icon: const Icon(Icons.open_in_new, size: 16),
              label: const Text('Swagger UI (/docs)'),
            ),
            OutlinedButton.icon(
              onPressed: () => open(Uri.parse('$baseUrl/openapi.json')),
              icon: const Icon(Icons.data_object, size: 16),
              label: const Text('OpenAPI 3.1 JSON'),
            ),
            OutlinedButton.icon(
              onPressed: () => open(Uri.parse('${AppConfig.repoUrl}/blob/main/docs/API.md')),
              icon: const Icon(Icons.rocket_launch_outlined, size: 16),
              label: const Text('Quickstart'),
            ),
            OutlinedButton.icon(
              onPressed: () => open(Uri.parse('${AppConfig.repoUrl}/tree/main/sdk')),
              icon: const Icon(Icons.extension_outlined, size: 16),
              label: const Text('Python & TypeScript SDKs'),
            ),
          ]),
          const SizedBox(height: 10),
          const Text(
            'Webhooks: POST /v1/developer/webhooks {url, events} for ride.completed, incident.opened and '
            'payout.sent, signed with X-HexaGo-Signature (HMAC-SHA256) and retried 3 times.',
            style: TextStyle(color: HexaColors.textMuted, fontSize: 12, height: 1.4),
          ),
        ],
      );
}
