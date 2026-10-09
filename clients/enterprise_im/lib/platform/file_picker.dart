import 'package:file_selector/file_selector.dart' as selector;

final class SelectedFile {
  final String name, mediaType;
  final int size;
  final Stream<List<int>> Function() open;
  const SelectedFile({
    required this.name,
    required this.mediaType,
    required this.size,
    required this.open,
  });
}

final class FilePicker {
  Future<SelectedFile?> pick() async {
    final file = await selector.openFile(
      acceptedTypeGroups: [
        const selector.XTypeGroup(
          label: '附件（PDF/PNG/JPEG/TXT）',
          extensions: ['pdf', 'png', 'jpg', 'jpeg', 'txt'],
          mimeTypes: [
            'application/pdf',
            'image/png',
            'image/jpeg',
            'text/plain',
          ],
          uniformTypeIdentifiers: [
            'com.adobe.pdf',
            'public.png',
            'public.jpeg',
            'public.plain-text',
          ],
        ),
      ],
    );
    if (file == null) return null;
    final ext = file.name.split('.').last.toLowerCase();
    final media = switch (ext) {
      'pdf' => 'application/pdf',
      'png' => 'image/png',
      'jpg' || 'jpeg' => 'image/jpeg',
      'txt' => 'text/plain',
      _ => '',
    };
    return SelectedFile(
      name: file.name,
      mediaType: media,
      size: await file.length(),
      open: file.openRead,
    );
  }
}
