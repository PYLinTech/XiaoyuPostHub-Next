import { fsApi, guestApi, mailApi } from "@/api/endpoints";
import type { DeliveryPlan, Purpose } from "@/api/types";
import type { ClientKeyPair } from "@/crypto/clientkey";
import type { DeliverySource } from "./download";

// 交付计划来源。
//
// 三种入口（登录用户自己的文件、分享访客、取件码）在服务端是三条路径，
// 但拿到计划之后的处理完全一致。这里把它们统一成 DeliverySource，
// 于是下载与预览的实现都不需要知道自己面对的是哪一种入口。

export function fileDeliverySource(path: string, purpose: Purpose): DeliverySource {
  return {
    plan: (pair: ClientKeyPair | null): Promise<DeliveryPlan> =>
      (purpose === "preview" ? fsApi.preview : fsApi.download)({
        path,
        clientPublicKey: pair?.publicKeyBase64 ?? "",
      }),
  };
}

export function shareDeliverySource(
  id: string,
  relPath: string,
  password: string,
  purpose: Purpose,
): DeliverySource {
  return {
    plan: (pair: ClientKeyPair | null) =>
      (purpose === "preview" ? guestApi.previewShare : guestApi.downloadShare)(id, {
        password,
        relPath,
        clientPublicKey: pair?.publicKeyBase64 ?? "",
      }),
  };
}

export function pickupDeliverySource(code: string): DeliverySource {
  return {
    plan: (pair: ClientKeyPair | null) =>
      guestApi.downloadPickup(code, pair?.publicKeyBase64 ?? ""),
  };
}

// mailPartDeliverySource 是邮件正文/内嵌图/附件的交付源。
//
// 用途在服务端按部件种类强制（body/inline=preview、attachment=download），
// 调用方不传 purpose——服务端本来就不采信它，留着只会让人以为这里有选择。
// 票据、三模式、结算走的完全是同一条 runDelivery 链路。
export function mailPartDeliverySource(partId: number): DeliverySource {
  return {
    plan: (pair: ClientKeyPair | null) =>
      mailApi.deliverPart(partId, pair?.publicKeyBase64 ?? ""),
  };
}
