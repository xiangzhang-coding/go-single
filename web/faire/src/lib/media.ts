const MAX_IMAGE_BYTES = 5 * 1024 * 1024;
const MAX_MESSAGE_FILE_BYTES = 20 * 1024 * 1024;
const IMAGE_TYPES = ["image/png", "image/jpeg", "image/webp", "image/gif"];
const MESSAGE_FILE_EXTENSIONS = ["pdf", "zip", "txt", "csv", "md"];

export const IMAGE_ACCEPT = IMAGE_TYPES.join(",");
export const MESSAGE_FILE_ACCEPT = MESSAGE_FILE_EXTENSIONS.map((extension) => `.${extension}`).join(",");

export function validateImage(file: File, subject = "图片"): string | null {
  if (!IMAGE_TYPES.includes(file.type)) {
    return `${subject}仅支持 png / jpeg / webp / gif。`;
  }
  if (file.size > MAX_IMAGE_BYTES) {
    return `${subject}不能超过 5MB。`;
  }
  return null;
}

export function validateMessageFile(file: File): string | null {
  const extension = file.name.split(".").pop()?.toLowerCase() || "";
  if (!MESSAGE_FILE_EXTENSIONS.includes(extension)) {
    return "文件仅支持 PDF、ZIP、TXT、CSV 和 Markdown。";
  }
  if (file.size > MAX_MESSAGE_FILE_BYTES) {
    return "文件不能超过 20MB。";
  }
  return null;
}
