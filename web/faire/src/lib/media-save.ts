export interface PreparedMediaUpload {
  file: File;
  reference: string;
}

export async function executeMediaSave<T>(
  file: File | null,
  prepared: PreparedMediaUpload | null,
  upload: (file: File) => Promise<{ url: string }>,
  save: (reference?: string) => Promise<T>,
  remember: (prepared: PreparedMediaUpload) => void,
): Promise<T> {
  let current = prepared?.file === file ? prepared : null;
  if (file && !current) {
    const uploaded = await upload(file);
    current = { file, reference: uploaded.url };
    remember(current);
  }
  return save(current?.reference);
}
