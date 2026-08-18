import { useEffect, useState, type ImgHTMLAttributes, type ReactNode } from "react";

import { getMedia } from "../api/endpoints";
import { ApiRequestError, getApiErrorMessage } from "../api/client";
import { Icon, Spinner } from "./ui";

interface AuthorizedImageProps extends Omit<ImgHTMLAttributes<HTMLImageElement>, "src"> {
  reference: string;
  fallback?: ReactNode;
}

export function AuthorizedImage({ reference, fallback, ...props }: AuthorizedImageProps) {
  const [source, setSource] = useState("");
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let active = true;
    let objectURL = "";
    setSource("");
    setFailed(false);
    void getMedia(reference)
      .then(({ blob }) => {
        objectURL = URL.createObjectURL(blob);
        if (active) {
          setSource(objectURL);
        } else {
          URL.revokeObjectURL(objectURL);
          objectURL = "";
        }
      })
      .catch(() => {
        if (active) setFailed(true);
      });
    return () => {
      active = false;
      if (objectURL) URL.revokeObjectURL(objectURL);
    };
  }, [reference]);

  if (failed || !source) return <>{fallback !== undefined ? fallback : <span className="media-placeholder">媒体不可用或无权访问</span>}</>;
  return <img {...props} src={source} />;
}

export function AuthorizedDownload({ reference }: { reference: string }) {
  const [downloading, setDownloading] = useState(false);
  const [error, setError] = useState("");

  async function download() {
    setDownloading(true);
    setError("");
    try {
      const { blob, filename } = await getMedia(reference);
      const objectURL = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = objectURL;
      link.download = filename;
      document.body.appendChild(link);
      link.click();
      link.remove();
      window.setTimeout(() => URL.revokeObjectURL(objectURL), 0);
    } catch (cause) {
      if (cause instanceof ApiRequestError && (cause.status === 403 || cause.status === 404)) {
        setError("文件不可用或授权已失效");
      } else {
        setError(getApiErrorMessage(cause, "文件下载失败"));
      }
    } finally {
      setDownloading(false);
    }
  }

  return (
    <span>
      <button type="button" className="chat-bubble-file" disabled={downloading} onClick={download}>
        {downloading ? <Spinner label="下载中" /> : <><Icon name="pin" size={15} /> 下载文件</>}
      </button>
      {error && <small className="media-download-error">{error}</small>}
    </span>
  );
}
