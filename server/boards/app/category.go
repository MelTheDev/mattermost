// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package app

import (
	"errors"
	"fmt"

	"github.com/mattermost/mattermost-server/server/v8/boards/model"
	"github.com/mattermost/mattermost-server/server/v8/boards/utils"
)

var errCategoryNotFound = errors.New("category ID specified in input does not exist for user")
var errCategoriesLengthMismatch = errors.New("cannot update category order, passed list of categories different size than in database")
var ErrCannotDeleteSystemCategory = errors.New("cannot delete a system category")
var ErrCannotUpdateSystemCategory = errors.New("cannot update a system category")

func (a *App) GetCategory(categoryID string) (*model.Category, error) {
	return a.store.GetCategory(categoryID)
}

func (a *App) CreateCategory(category *model.Category) (*model.Category, error) {
	category.Hydrate()
	if err := category.IsValid(); err != nil {
		return nil, err
	}

	if err := a.store.CreateCategory(*category); err != nil {
		return nil, err
	}

	createdCategory, err := a.store.GetCategory(category.ID)
	if err != nil {
		return nil, err
	}

	go func() {
		a.wsAdapter.BroadcastCategoryChange(*createdCategory)
	}()

	return createdCategory, nil
}

func (a *App) UpdateCategory(category *model.Category) (*model.Category, error) {
	category.Hydrate()

	if err := category.IsValid(); err != nil {
		return nil, err
	}

	// verify if category belongs to the user
	existingCategory, err := a.store.GetCategory(category.ID)
	if err != nil {
		return nil, err
	}

	if existingCategory.DeleteAt != 0 {
		return nil, model.ErrCategoryDeleted
	}

	if existingCategory.UserID != category.UserID {
		return nil, model.ErrCategoryPermissionDenied
	}

	if existingCategory.TeamID != category.TeamID {
		return nil, model.ErrCategoryPermissionDenied
	}

	// in case type was defaulted above, set to existingCategory.Type
	category.Type = existingCategory.Type
	if existingCategory.Type == model.CategoryTypeSystem {
		// You cannot rename or delete a system category,
		// So restoring its name and undeleting it if set so.
		category.Name = existingCategory.Name
		category.DeleteAt = 0
	}

	category.UpdateAt = utils.GetMillis()
	if err = category.IsValid(); err != nil {
		return nil, err
	}
	if err = a.store.UpdateCategory(*category); err != nil {
		return nil, err
	}

	updatedCategory, err := a.store.GetCategory(category.ID)
	if err != nil {
		return nil, err
	}

	go func() {
		a.wsAdapter.BroadcastCategoryChange(*updatedCategory)
	}()

	return updatedCategory, nil
}

func (a *App) DeleteCategory(categoryID, userID, teamID string) (*model.Category, error) {
	existingCategory, err := a.store.GetCategory(categoryID)
	if err != nil {
		return nil, err
	}

	// category is already deleted. This avoids
	// overriding the original deleted at timestamp
	if existingCategory.DeleteAt != 0 {
		return existingCategory, nil
	}

	// verify if category belongs to the user
	if existingCategory.UserID != userID {
		return nil, model.ErrCategoryPermissionDenied
	}

	// verify if category belongs to the team
	if existingCategory.TeamID != teamID {
		return nil, model.NewErrInvalidCategory("category doesn't belong to the team")
	}

	if existingCategory.Type == model.CategoryTypeSystem {
		return nil, ErrCannotDeleteSystemCategory
	}

	if err = a.moveBoardsToDefaultCategory(userID, teamID, categoryID); err != nil {
		return nil, err
	}

	if err = a.store.DeleteCategory(categoryID, userID, teamID); err != nil {
		return nil, err
	}

	deletedCategory, err := a.store.GetCategory(categoryID)
	if err != nil {
		return nil, err
	}

	go func() {
		a.wsAdapter.BroadcastCategoryChange(*deletedCategory)
	}()

	return deletedCategory, nil
}

func (a *App) moveBoardsToDefaultCategory(userID, teamID, sourceCategoryID string) error {
	var sourceCategoryBoards *model.CategoryBoards
	defaultCategoryID := ""

	// iterate user's categories to find the source category
	// and the default category.
	// We need source category to get the list of its board
	// and the default category to know its ID to
	// move source category's boards to.
	err := a.ForEachUserCategoryBoard(userID, teamID, func(categoryBoards model.CategoryBoards) (bool, error) {
		if categoryBoards.ID == sourceCategoryID {
			sourceCategoryBoards = &categoryBoards
		}

		if categoryBoards.Name == defaultCategoryBoards {
			defaultCategoryID = categoryBoards.ID
		}

		// if both categories are found, no need to iterate furthur.
		if sourceCategoryBoards != nil && defaultCategoryID != "" {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return err
	}

	if sourceCategoryBoards == nil {
		return errCategoryNotFound
	}

	if defaultCategoryID == "" {
		return fmt.Errorf("moveBoardsToDefaultCategory: %w", errNoDefaultCategoryFound)
	}

	boardIDs := make([]string, len(sourceCategoryBoards.BoardMetadata))
	for i := range sourceCategoryBoards.BoardMetadata {
		boardIDs[i] = sourceCategoryBoards.BoardMetadata[i].BoardID
	}

	if err := a.AddUpdateUserCategoryBoard(teamID, userID, defaultCategoryID, boardIDs); err != nil {
		return fmt.Errorf("moveBoardsToDefaultCategory: %w", err)
	}

	return nil
}

func (a *App) ReorderCategories(userID, teamID string, newCategoryOrder []string) ([]string, error) {
	if err := a.verifyNewCategoriesMatchExisting(userID, teamID, newCategoryOrder); err != nil {
		return nil, err
	}

	newOrder, err := a.store.ReorderCategories(userID, teamID, newCategoryOrder)
	if err != nil {
		return nil, err
	}

	go func() {
		a.wsAdapter.BroadcastCategoryReorder(teamID, userID, newOrder)
	}()

	return newOrder, nil
}

func (a *App) verifyNewCategoriesMatchExisting(userID, teamID string, newCategoryOrder []string) error {
	existingCategoriesMap := map[string]struct{}{}

	err := a.ForEachUserCategory(userID, teamID, func(category model.Category) (bool, error) {
		existingCategoriesMap[category.ID] = struct{}{}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("error fetching user categories: %w", err)
	}

	if len(newCategoryOrder) != len(existingCategoriesMap) {
		return fmt.Errorf(
			"%w length new categories: %d, length existing categories: %d, userID: %s, teamID: %s",
			errCategoriesLengthMismatch,
			len(newCategoryOrder),
			len(existingCategoriesMap),
			userID,
			teamID,
		)
	}

	for _, newCategoryID := range newCategoryOrder {
		if _, found := existingCategoriesMap[newCategoryID]; !found {
			return fmt.Errorf(
				"%w specified category ID: %s, userID: %s, teamID: %s",
				errCategoryNotFound,
				newCategoryID,
				userID,
				teamID,
			)
		}
	}

	return nil
}

func (a *App) ForEachUserCategory(userID, teamID string, fn func(category model.Category) (bool, error)) error {
	page := 0
	const perPage = 50
	for ; true; page++ {
		categories, err := a.store.GetUserCategories(userID, teamID, model.QueryUserCategoriesOptions{
			Page:    page,
			PerPage: perPage,
		})
		if err != nil {
			return err
		}
		for _, category := range categories {
			if done, err := fn(category); err != nil || done {
				return err
			}
		}
		if len(categories) < perPage {
			break
		}
	}
	return nil
}
