import { create } from "@bufbuild/protobuf";
import { attachmentServiceClient } from "@/connect";
import type { Attachment } from "@/types/proto/api/v1/attachment_service_pb";
import { AttachmentSchema, MotionMediaSchema } from "@/types/proto/api/v1/attachment_service_pb";
import { type LocalFile, snapshotLocalFile } from "../types/attachment";

export const uploadService = {
  async uploadFiles(localFiles: LocalFile[]): Promise<Attachment[]> {
    if (localFiles.length === 0) return [];

    // Snapshot any file that is not already buffered (e.g. audio recordings),
    // and fail fast BEFORE uploading anything if a handle is unreadable — so a
    // bad file never leaves orphaned uploaded attachments behind.
    const withData = await Promise.all(localFiles.map(snapshotLocalFile));
    const unreadable = withData.filter((item) => !item.data);
    if (unreadable.length > 0) {
      throw new Error(
        `Cannot read file(s): ${unreadable.map((item) => item.file.name).join(", ")}. The file is no longer readable — remove it and re-select it, then save again.`,
      );
    }

    const attachments: Attachment[] = [];

    for (const localFile of withData) {
      const { motionMedia } = localFile;
      const attachment = await attachmentServiceClient.createAttachment({
        attachment: create(AttachmentSchema, {
          filename: localFile.file.name,
          size: BigInt(localFile.file.size),
          type: localFile.file.type,
          content: localFile.data as Uint8Array,
          motionMedia: motionMedia ? create(MotionMediaSchema, motionMedia) : undefined,
        }),
      });
      attachments.push(attachment);
    }

    return attachments;
  },
};
