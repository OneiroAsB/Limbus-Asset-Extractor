package extractor

import "fmt"

// classIDNames covers the classes people actually run into when poking
// around asset bundles. Anything missing just shows as "ClassID <n>" -
// this is a convenience table, not a complete copy of Unity's enum.
var classIDNames = map[int32]string{
	1:    "GameObject",
	4:    "Transform",
	21:   "Material",
	23:   "MeshRenderer",
	25:   "Renderer",
	28:   "Texture2D",
	33:   "MeshFilter",
	43:   "Mesh",
	45:   "Shader",
	48:   "Shader",
	49:   "TextAsset",
	74:   "AnimationClip",
	83:   "AudioClip",
	91:   "AnimatorController",
	95:   "Animator",
	111:  "Animation",
	114:  "MonoBehaviour",
	115:  "MonoScript",
	128:  "Font",
	142:  "AssetBundle",
	150:  "PrefabInstance",
	152:  "Cubemap",
	156:  "TerrainData",
	187:  "AudioSource",
	213:  "Sprite",
	224:  "RectTransform",
	226:  "SpriteAtlas",
	289:  "VideoClip",
	319:  "AssetImporterLog",
	329:  "PropertyModification",
	331:  "MonoImporter",
	1001: "PrefabRoot",
}

func classIDName(id int32) string {
	if n, ok := classIDNames[id]; ok {
		return n
	}
	return fmt.Sprintf("ClassID %d", id)
}
