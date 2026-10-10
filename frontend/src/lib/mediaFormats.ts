const types: Record<string, string> = {
  mp4: "video/mp4", m4v: "video/mp4", mov: "video/quicktime", "3gp": "video/3gpp", "3g2": "video/3gpp2",
  webm: "video/webm", mkv: "video/x-matroska", ogv: "video/ogg", mts: "video/mp2t", m2ts: "video/mp2t",
  mp3: "audio/mpeg", wav: "audio/wav", wave: "audio/wav", m4a: "audio/mp4", flac: "audio/flac",
  aac: "audio/aac", adts: "audio/aac", ogg: "audio/ogg", oga: "audio/ogg", opus: "audio/ogg", spx: "audio/ogg",
};

export function mediaMimeType(name: string, declared = ""): string {
  const type = declared.split(";")[0].trim().toLowerCase();
  if (type.startsWith("audio/") || type.startsWith("video/")) return declared.trim();
  return types[name.split(".").at(-1)?.toLowerCase() || ""] || declared;
}

export function mediaKind(name: string, declared = ""): "audio" | "video" | null {
  const type = mediaMimeType(name, declared).toLowerCase();
  return type.startsWith("audio/") ? "audio" : type.startsWith("video/") ? "video" : null;
}
