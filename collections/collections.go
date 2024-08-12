package collections

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"hmruntime/collections/in_mem"
	"hmruntime/collections/index"
	collection_utils "hmruntime/collections/utils"
	"hmruntime/functions"
	"hmruntime/manifestdata"
	"hmruntime/plugins"
	"hmruntime/utils"
	"hmruntime/wasmhost"

	wasm "github.com/tetratelabs/wazero/api"
)

func UpsertToCollection(ctx context.Context, collectionName, namespace string, keys, texts []string, labels [][]string) (*CollectionMutationResult, error) {

	// Get the collectionName data from the manifest
	collectionData := manifestdata.GetManifest().Collections[collectionName]

	collection, err := GlobalNamespaceManager.FindCollection(ctx, collectionName)
	if err != nil {
		return nil, err
	}

	if namespace == "" {
		namespace = in_mem.DefaultNamespace
	}

	collNs, err := collection.FindOrCreateNamespace(ctx, namespace, in_mem.NewCollectionNamespace(collectionName, namespace))
	if err != nil {
		return nil, err
	}

	if len(keys) == 0 {
		keys = make([]string, len(texts))
		for i := range keys {
			keys[i] = utils.GenerateUUIDv7()
		}
	}

	if len(labels) != 0 && len(labels) != len(texts) {
		return nil, fmt.Errorf("mismatch in number of labels and texts: %d != %d", len(labels), len(texts))
	}

	err = collNs.InsertTexts(ctx, keys, texts, labels)
	if err != nil {
		return nil, err
	}

	// compute embeddings for each search method, and insert into vector index
	for searchMethodName, searchMethod := range collectionData.SearchMethods {
		vectorIndex, err := collNs.GetVectorIndex(ctx, searchMethodName)
		if err == index.ErrVectorIndexNotFound {
			vectorIndex, err = createIndexObject(searchMethod, searchMethodName)
			if err != nil {
				return nil, err
			}
			err = collNs.SetVectorIndex(ctx, searchMethodName, vectorIndex)
			if err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}

		embedder := searchMethod.Embedder
		if err := validateEmbedder(ctx, embedder); err != nil {
			return nil, err
		}

		executionInfo, err := wasmhost.CallFunction(ctx, embedder, texts)
		if err != nil {
			return nil, err
		}

		result := executionInfo.Result

		textVecs, err := collection_utils.ConvertToFloat32_2DArray(result)
		if err != nil {
			return nil, err
		}

		if len(textVecs) != len(texts) {
			return nil, fmt.Errorf("mismatch in number of embeddings generated by embedder %s", embedder)
		}

		ids := make([]int64, len(keys))
		for i := range textVecs {
			key := keys[i]

			id, err := collNs.GetExternalId(ctx, key)
			if err != nil {
				return nil, err
			}
			ids[i] = id
		}

		err = vectorIndex.InsertVectors(ctx, ids, textVecs)
		if err != nil {
			return nil, err
		}
	}

	return &CollectionMutationResult{
		Collection: collectionName,
		Operation:  "upsert",
		Status:     "success",
		Keys:       keys,
		Error:      "",
	}, nil
}

var errInvalidEmbedderSignature = errors.New("invalid embedder function signature")

func validateEmbedder(ctx context.Context, embedder string) error {

	fn, err := functions.GetFunction(embedder)
	if err != nil {
		return err
	}

	// Embedder functions must take a single string[] parameter and return a single f32[][] or f64[][] array.
	// The types are language-specific, so we use the plugin language's type info.

	if len(fn.Parameters) != 1 || len(fn.Results) != 1 {
		return errInvalidEmbedderSignature
	}

	lti := plugins.GetPlugin(ctx).Language.TypeInfo()

	p := fn.Parameters[0]
	if !lti.IsArrayType(p.Type) || !lti.IsStringType(lti.GetArraySubtype(p.Type)) {
		return errInvalidEmbedderSignature
	}

	r := fn.Results[0]
	if !lti.IsArrayType(r.Type) {
		return errInvalidEmbedderSignature
	}

	a := lti.GetArraySubtype(r.Type)
	if !lti.IsArrayType(a) || !lti.IsFloatType(lti.GetArraySubtype(a)) {
		return errInvalidEmbedderSignature
	}

	return nil
}

func DeleteFromCollection(ctx context.Context, collectionName, namespace, key string) (*CollectionMutationResult, error) {
	collection, err := GlobalNamespaceManager.FindCollection(ctx, collectionName)
	if err != nil {
		return nil, err
	}

	if namespace == "" {
		namespace = in_mem.DefaultNamespace
	}

	collNs, err := collection.FindNamespace(ctx, namespace)
	if err != nil {
		return nil, err
	}

	textId, err := collNs.GetExternalId(ctx, key)
	if err != nil {
		return nil, err
	}
	for _, vectorIndex := range collNs.GetVectorIndexMap() {
		err = vectorIndex.DeleteVector(ctx, textId, key)
		if err != nil {
			return nil, err
		}
	}
	err = collNs.DeleteText(ctx, key)
	if err != nil {
		return nil, err
	}

	keys := []string{key}

	return &CollectionMutationResult{
		Collection: collectionName,
		Operation:  "delete",
		Status:     "success",
		Keys:       keys,
		Error:      "",
	}, nil

}

func getEmbedder(ctx context.Context, collectionName string, searchMethod string) (string, error) {
	manifestColl, ok := manifestdata.GetManifest().Collections[collectionName]
	if !ok {
		return "", fmt.Errorf("collection %s not found in manifest", collectionName)
	}

	manifestSearchMethod, ok := manifestColl.SearchMethods[searchMethod]
	if !ok {
		return "", fmt.Errorf("search method %s not found in collection %s", searchMethod, collectionName)
	}

	embedder := manifestSearchMethod.Embedder
	if embedder == "" {
		return "", fmt.Errorf("embedder not found in search method %s of collection %s", searchMethod, collectionName)
	}

	if err := validateEmbedder(ctx, embedder); err != nil {
		return "", err
	}

	return embedder, nil
}

func SearchCollection(ctx context.Context, collectionName string, namespaces []string, searchMethod, text string, limit int32, returnText bool) (*CollectionSearchResult, error) {

	collection, err := GlobalNamespaceManager.FindCollection(ctx, collectionName)
	if err != nil {
		return nil, err
	}

	if len(namespaces) == 0 {
		namespaces = []string{in_mem.DefaultNamespace}
	}

	embedder, err := getEmbedder(ctx, collectionName, searchMethod)
	if err != nil {
		return nil, err
	}

	texts := []string{text}

	executionInfo, err := wasmhost.CallFunction(ctx, embedder, texts)
	if err != nil {
		return nil, err
	}

	result := executionInfo.Result

	textVecs, err := collection_utils.ConvertToFloat32_2DArray(result)
	if err != nil {
		return nil, err
	}

	if len(textVecs) == 0 {
		return nil, fmt.Errorf("no embeddings generated by embedder %s", embedder)
	}

	// merge all objects
	mergedObjects := make([]*CollectionSearchResultObject, 0, len(namespaces)*int(limit))
	for _, ns := range namespaces {
		collNs, err := collection.FindNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}

		vectorIndex, err := collNs.GetVectorIndex(ctx, searchMethod)
		if err != nil {
			return nil, err
		}

		objects, err := vectorIndex.Search(ctx, textVecs[0], int(limit), nil)
		if err != nil {
			return nil, err
		}

		for _, object := range objects {
			text, err := collNs.GetText(ctx, object.GetIndex())
			if err != nil {
				return nil, err
			}
			mergedObjects = append(mergedObjects, &CollectionSearchResultObject{
				Namespace: ns,
				Key:       object.GetIndex(),
				Text:      text,
				Distance:  object.GetValue(),
				Score:     1 - object.GetValue(),
			})
		}
	}

	// sort by score
	sort.Slice(mergedObjects, func(i, j int) bool {
		return mergedObjects[i].Distance < mergedObjects[j].Distance
	})

	mergedObjects = mergedObjects[:int(limit)]

	return &CollectionSearchResult{
		Collection:   collectionName,
		SearchMethod: searchMethod,
		Status:       "success",
		Objects:      mergedObjects,
	}, nil

}

func NnClassify(ctx context.Context, collectionName, namespace, searchMethod, text string) (*CollectionClassificationResult, error) {

	collection, err := GlobalNamespaceManager.FindCollection(ctx, collectionName)
	if err != nil {
		return nil, err
	}

	if namespace == "" {
		namespace = in_mem.DefaultNamespace
	}

	collNs, err := collection.FindNamespace(ctx, namespace)
	if err != nil {
		return nil, err
	}

	vectorIndex, err := collNs.GetVectorIndex(ctx, searchMethod)
	if err != nil {
		return nil, err
	}

	embedder, err := getEmbedder(ctx, collectionName, searchMethod)
	if err != nil {
		return nil, err
	}

	texts := []string{text}

	executionInfo, err := wasmhost.CallFunction(ctx, embedder, texts)
	if err != nil {
		return nil, err
	}

	result := executionInfo.Result

	textVecs, err := collection_utils.ConvertToFloat32_2DArray(result)
	if err != nil {
		return nil, err
	}

	if len(textVecs) == 0 {
		return nil, fmt.Errorf("no embeddings generated by embedder %s", embedder)
	}

	lenTexts, err := collNs.Len(ctx)
	if err != nil {
		return nil, err
	}

	nns, err := vectorIndex.Search(ctx, textVecs[0], int(math.Log10(float64(lenTexts)))*int(math.Log10(float64(lenTexts))), nil)
	if err != nil {
		return nil, err
	}

	// remove elements with score out of first standard deviation

	// calculate mean
	var sum float64
	for _, nn := range nns {
		sum += nn.GetValue()
	}
	mean := sum / float64(len(nns))

	// calculate standard deviation
	var variance float64
	for _, nn := range nns {
		variance += math.Pow(float64(nn.GetValue())-mean, 2)
	}
	stdDev := math.Sqrt(variance / float64(len(nns)))

	// remove elements with score out of first standard deviation and return the most frequent label
	labelCounts := make(map[string]int)

	res := &CollectionClassificationResult{
		Collection:   collectionName,
		LabelsResult: make([]*CollectionClassificationLabelObject, 0),
		SearchMethod: searchMethod,
		Status:       "success",
		Cluster:      make([]*CollectionClassificationResultObject, 0),
	}

	totalLabels := 0

	for _, nn := range nns {
		if math.Abs(nn.GetValue()-mean) <= 2*stdDev {
			labels, err := collNs.GetLabels(ctx, nn.GetIndex())
			if err != nil {
				return nil, err
			}
			for _, label := range labels {
				labelCounts[label]++
				totalLabels++
			}

			res.Cluster = append(res.Cluster, &CollectionClassificationResultObject{
				Key:      nn.GetIndex(),
				Labels:   labels,
				Score:    1 - nn.GetValue(),
				Distance: nn.GetValue(),
			})
		}
	}

	// Create a slice of pairs
	labelsResult := make([]*CollectionClassificationLabelObject, 0, len(labelCounts))
	for label, count := range labelCounts {
		labelsResult = append(labelsResult, &CollectionClassificationLabelObject{
			Label:      label,
			Confidence: float64(count) / float64(totalLabels),
		})
	}

	// Sort the pairs by count in descending order
	sort.Slice(labelsResult, func(i, j int) bool {
		return labelsResult[i].Confidence > labelsResult[j].Confidence
	})

	res.LabelsResult = labelsResult

	return res, nil
}

func ComputeDistance(ctx context.Context, collectionName, namespace, searchMethod, id1, id2 string) (*CollectionSearchResultObject, error) {

	collection, err := GlobalNamespaceManager.FindCollection(ctx, collectionName)
	if err != nil {
		return nil, err
	}

	if namespace == "" {
		namespace = in_mem.DefaultNamespace
	}

	collNs, err := collection.FindNamespace(ctx, namespace)
	if err != nil {
		return nil, err
	}

	vectorIndex, err := collNs.GetVectorIndex(ctx, searchMethod)
	if err != nil {
		return nil, err
	}

	vec1, err := vectorIndex.GetVector(ctx, id1)
	if err != nil {
		return nil, err
	}

	vec2, err := vectorIndex.GetVector(ctx, id2)
	if err != nil {
		return nil, err
	}

	distance, err := collection_utils.CosineDistance(vec1, vec2)
	if err != nil {
		return nil, err
	}

	return &CollectionSearchResultObject{
		Namespace: namespace,
		Key:       "",
		Text:      "",
		Distance:  distance,
		Score:     1 - distance,
	}, nil
}

func RecomputeSearchMethod(ctx context.Context, mod wasm.Module, collectionName, namespace, searchMethod string) (*SearchMethodMutationResult, error) {

	collection, err := GlobalNamespaceManager.FindCollection(ctx, collectionName)
	if err != nil {
		return nil, err
	}

	if namespace == "" {
		namespace = in_mem.DefaultNamespace
	}

	collNs, err := collection.FindNamespace(ctx, namespace)
	if err != nil {
		return nil, err
	}

	vectorIndex, err := collNs.GetVectorIndex(ctx, searchMethod)
	if err != nil {
		return nil, err
	}

	embedder, err := getEmbedder(ctx, collectionName, searchMethod)
	if err != nil {
		return nil, err
	}

	err = ProcessTextMapWithModule(ctx, mod, collNs, embedder, vectorIndex)
	if err != nil {
		return nil, err
	}

	return &SearchMethodMutationResult{
		Collection: collectionName,
		Operation:  "recompute",
		Status:     "success",
		Error:      "",
	}, nil

}

func GetTextFromCollection(ctx context.Context, collectionName, namespace, key string) (string, error) {
	collection, err := GlobalNamespaceManager.FindCollection(ctx, collectionName)
	if err != nil {
		return "", err
	}

	collNs, err := collection.FindNamespace(ctx, namespace)
	if err != nil {
		return "", err
	}

	text, err := collNs.GetText(ctx, key)
	if err != nil {
		return "", err
	}

	return text, nil
}

func GetTextsFromCollection(ctx context.Context, collectionName, namespace string) (map[string]string, error) {

	collection, err := GlobalNamespaceManager.FindCollection(ctx, collectionName)
	if err != nil {
		return nil, err
	}

	if namespace == "" {
		namespace = in_mem.DefaultNamespace
	}

	collNs, err := collection.FindNamespace(ctx, namespace)
	if err != nil {
		return nil, err
	}

	textMap, err := collNs.GetTextMap(ctx)
	if err != nil {
		return nil, err
	}

	return textMap, nil
}

func GetNamespacesFromCollection(ctx context.Context, collectionName string) ([]string, error) {
	collection, err := GlobalNamespaceManager.FindCollection(ctx, collectionName)
	if err != nil {
		return nil, err
	}

	namespaceMap := collection.GetCollectionNamespaceMap()

	namespaces := make([]string, 0, len(namespaceMap))
	for namespace := range namespaceMap {
		namespaces = append(namespaces, namespace)
	}

	return namespaces, nil
}
